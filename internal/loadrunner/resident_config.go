package loadrunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	MaxResidentPopulation       = 210_000
	MaxSharedResidentPopulation = 100
	sharedResidentLockBucket    = "rayai-dev-api-canary-locks"
)

type ResidentConfig struct {
	Population    int
	Stages        []int
	Hold          time.Duration
	ProbeInterval time.Duration
	JournalPath   string
	PreflightPath string
	InventoryPath string
	Mode          string
	Preflight     ResidentPreflight
}

// ResidentPreflight records operator checks unavailable through the public API.
// It is evidence for one staging run, never production admission.
type ResidentPreflight struct {
	RunID                 string    `json:"run_id"`
	TeamID                string    `json:"team_id"`
	Target                string    `json:"target"`
	ExpectedHost          string    `json:"expected_host"`
	Template              string    `json:"template"`
	ReviewedAt            time.Time `json:"reviewed_at"`
	ValidUntil            time.Time `json:"valid_until"`
	CapacityPopulation    int       `json:"capacity_population"`
	CapacityConcurrency   int       `json:"capacity_concurrency"`
	IsolatedHost          bool      `json:"isolated_host"`
	SharedHostSmall       bool      `json:"shared_host_small"`
	QuotaChecked          bool      `json:"quota_checked"`
	CreditsChecked        bool      `json:"credits_checked"`
	DiskCapacityChecked   bool      `json:"disk_capacity_checked"`
	RetentionChecked      bool      `json:"retention_checked"`
	BillingWritesChecked  bool      `json:"billing_writes_checked"`
	TelemetryChecked      bool      `json:"telemetry_checked"`
	ControlPlaneRevision  string    `json:"control_plane_revision"`
	HostRevision          string    `json:"host_revision"`
	ComparisonRevision    string    `json:"comparison_revision"`
	MaxPhaseLatencyMillis int64     `json:"max_phase_latency_ms"`
	MaxProbeFailures      int       `json:"max_probe_failures"`
}

func loadResidentConfig(cfg Config) (ResidentConfig, error) {
	r := ResidentConfig{
		Population:    envInt("LOAD_TEST_RESIDENT_POPULATION", 0),
		Hold:          envDuration("LOAD_TEST_RESIDENT_HOLD", 15*time.Minute),
		ProbeInterval: envDuration("LOAD_TEST_RESIDENT_PROBE_INTERVAL", 30*time.Second),
		JournalPath:   strings.TrimSpace(os.Getenv("LOAD_TEST_RESIDENT_JOURNAL")),
		PreflightPath: strings.TrimSpace(os.Getenv("LOAD_TEST_RESIDENT_PREFLIGHT")),
		InventoryPath: strings.TrimSpace(os.Getenv("LOAD_TEST_RESIDENT_INVENTORY_EVIDENCE")),
		Mode:          envDefault("LOAD_TEST_RESIDENT_MODE", "qualify"),
	}
	if !strings.EqualFold(os.Getenv("LOAD_TEST_RESIDENT_OPT_IN"), "true") {
		return r, errors.New("resident qualification requires LOAD_TEST_RESIDENT_OPT_IN=true")
	}
	file, err := os.Open(r.PreflightPath)
	if err != nil {
		return r, errors.New("resident qualification requires a readable preflight evidence file")
	}
	defer file.Close()
	dec := json.NewDecoder(io.LimitReader(file, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r.Preflight); err != nil {
		return r, fmt.Errorf("resident preflight: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return r, errors.New("resident preflight must contain one JSON object")
	}
	for _, raw := range strings.Split(os.Getenv("LOAD_TEST_RESIDENT_STAGES"), ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return r, errors.New("resident stages must be increasing population counts")
		}
		r.Stages = append(r.Stages, n)
	}
	if len(r.Stages) == 0 {
		r.Stages = []int{r.Population}
	}
	if r.Hold < 15*time.Minute || r.Hold > time.Hour || r.ProbeInterval < time.Second || r.ProbeInterval > time.Minute {
		return r, errors.New("resident hold must cover at least three 5-minute cycles and at most one hour; probe interval must be 1s..1m")
	}
	if err := validateResident(cfg, r, time.Now()); err != nil {
		return r, err
	}
	return r, nil
}

func validateResident(cfg Config, r ResidentConfig, now time.Time) error {
	cfg.Resident = r
	if cfg.Environment != "staging" || cfg.Region != "us-central1" || cfg.Target != stagingTarget || cfg.APIBaseURL != stagingAPIBaseURL || cfg.PreviewDomain != stagingPreviewDomain || cfg.Scheduled {
		return errors.New("resident qualification is manual and restricted to the staging target")
	}
	p := r.Preflight
	if cfg.SlackWebhookURL != "" {
		return errors.New("resident qualification requires no summary webhook")
	}
	outerLock := cfg.LockFile
	if p.SharedHostSmall {
		if p.IsolatedHost || r.Population > MaxSharedResidentPopulation || cfg.Concurrency > 2 {
			return errors.New("shared-host-small requires a shared host, population at most 100 and concurrency at most 2")
		}
		if r.Mode == "seed" {
			return errors.New("shared-host-small cannot retain seeded residents after releasing shared guards; use qualify or cleanup")
		}
		if cfg.LockBackend != "gcs" || cfg.LockBucket != sharedResidentLockBucket || !cfg.BenchmarkGuard || !cfg.GlobalLock {
			return errors.New("shared-host-small requires the deployed staging GCS bucket and benchmark/global guards")
		}
		outerLock = ""
	} else if !p.IsolatedHost || cfg.LockBackend != "file" || cfg.LockFile == "" {
		return errors.New("isolated resident qualification requires an isolated host and local file locking")
	}
	if r.Mode != "seed" && r.Mode != "qualify" && r.Mode != "cleanup" {
		return errors.New("resident mode must be seed, qualify, or cleanup")
	}
	if r.Population < 1 || r.Population > MaxResidentPopulation || cfg.Concurrency < 1 || cfg.Concurrency > 64 || r.JournalPath == "" || cfg.RunID == "" || cfg.WorkerID == "" {
		return errors.New("resident population must be 1..210000, concurrency 1..64, with explicit journal and ownership")
	}
	if err := validateResidentPaths(r.JournalPath, outerLock, r.PreflightPath, r.InventoryPath); err != nil {
		return err
	}
	last := 0
	for _, stage := range r.Stages {
		if stage <= last || stage > r.Population {
			return errors.New("resident stages must increase to the configured population")
		}
		last = stage
	}
	if last != r.Population {
		return errors.New("last resident stage must equal population")
	}
	if _, err := uuid.Parse(p.TeamID); err != nil {
		return errors.New("resident preflight requires an explicit staging team UUID")
	}
	if p.RunID != cfg.RunID || p.Target != cfg.Target || p.ExpectedHost == "" || p.Template != cfg.Template {
		return errors.New("resident preflight does not match run, target, host and template")
	}
	if cfg.RunTimeout <= 0 || cfg.RunTimeout > maxRunTimeout || cfg.DeleteTimeout <= 0 || cfg.DeleteTimeout > maxDeleteTimeout {
		return errors.New("resident runtime and per-owner deletion timeout must fit their safety bounds")
	}
	if err := validateExecutionConfig(cfg); err != nil {
		return err
	}
	if r.Mode == "cleanup" {
		return nil
	}
	if r.InventoryPath == "" {
		return errors.New("resident qualification requires a continuously refreshed authoritative inventory evidence file")
	}
	if !p.QuotaChecked || !p.CreditsChecked || !p.DiskCapacityChecked || !p.RetentionChecked || !p.BillingWritesChecked || !p.TelemetryChecked || p.CapacityPopulation < r.Population+cfg.Concurrency || p.CapacityConcurrency < cfg.Concurrency {
		return errors.New("resident preflight must confirm quota, credits, disk, retention, billing writes, telemetry and resident plus probe capacity")
	}
	if p.ReviewedAt.IsZero() || p.ReviewedAt.After(now) || now.Sub(p.ReviewedAt) > 30*time.Minute || p.ValidUntil.Before(now.Add(cfg.RunTimeout+cleanupTimeout(cfg))) {
		return errors.New("resident capacity evidence is stale or does not cover runtime and cleanup")
	}
	for _, revision := range []string{p.ControlPlaneRevision, p.HostRevision, p.ComparisonRevision} {
		if len(revision) != 40 || strings.Trim(revision, "0123456789abcdef") != "" {
			return errors.New("resident preflight requires exact control-plane, host and comparison revisions")
		}
	}
	if p.MaxPhaseLatencyMillis <= 0 || p.MaxPhaseLatencyMillis > int64((10*time.Minute)/time.Millisecond) || p.MaxProbeFailures < 0 || cfg.Operations < 1 || cfg.Operations > MaxOperations || r.Hold <= 0 || r.ProbeInterval <= 0 || cfg.RunTimeout <= 0 || cfg.DeleteTimeout <= 0 || cfg.ActiveTimeout <= 0 || cfg.PollInterval <= 0 {
		return errors.New("resident qualification requires latency/failure abort thresholds and bounded probe budget")
	}
	if r.Mode == "qualify" && int64(cfg.Operations) < int64(len(r.Stages))*int64((r.Hold+r.ProbeInterval-1)/r.ProbeInterval) {
		return errors.New("probe operation budget cannot cover all resident hold stages")
	}
	if r.Mode == "qualify" && cfg.RunTimeout < time.Duration(len(r.Stages))*r.Hold {
		return errors.New("resident runtime cannot cover all hold stages")
	}
	return nil
}

// ValidateResidentAdmission runs before acquiring the outer file lock, whose
// metadata write truncates that lock file. A lock must never alias the journal.
func ValidateResidentAdmission(cfg Config, now time.Time) error {
	if cfg.Scenario != "resident" {
		return nil
	}
	return validateResident(cfg, cfg.Resident, now)
}

func validateResidentPaths(journal, outerLock string, evidence ...string) error {
	paths := []string{journal, journal + ".owner-lock"}
	if outerLock != "" {
		paths = append(paths, outerLock)
	}
	for _, path := range evidence {
		if path != "" {
			paths = append(paths, path)
		}
	}
	canonical := make([]string, len(paths))
	infos := make([]os.FileInfo, len(paths))
	for i, path := range paths {
		var err error
		canonical[i], err = residentCanonicalPath(path)
		if err != nil {
			return fmt.Errorf("resident state path: %w", err)
		}
		infos[i], err = os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for prior := 0; prior < i; prior++ {
			if canonical[i] == canonical[prior] || infos[i] != nil && infos[prior] != nil && os.SameFile(infos[i], infos[prior]) {
				return errors.New("resident journal, owner lock and outer lock must be distinct files")
			}
		}
	}
	return nil
}

func residentCanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	ancestor := abs
	tail := []string{}
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		tail = append(tail, filepath.Base(ancestor))
		ancestor = parent
	}
}
