package loadrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

type residentClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *residentClock) read() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *residentClock) wait(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

type residentFake struct {
	lifecycle.Client
	mu             sync.Mutex
	clock          *residentClock
	items          map[string]canaryapi.Sandbox
	creates        int
	deleted        []string
	host           string
	maxPaused      int
	probeError     error
	unknownCreate  bool
	lookupFailure  bool
	resumeDuration time.Duration
	resumeHost     string
	badOrphanLimit bool
}

func (f *residentFake) hint(id string) string {
	data, _ := json.Marshal(map[string]any{"s": id, "h": f.host, "a": stagingPreviewDomain, "e": f.clock.read().Add(time.Hour).Unix(), "v": 1})
	return "v1." + base64.RawURLEncoding.EncodeToString(data) + ".opaque-server-signature"
}
func (f *residentFake) CreateSandbox(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if req.TimeoutSeconds != 60 || req.IdleTimeoutSeconds == nil || *req.IdleTimeoutSeconds != 60 {
		f.badOrphanLimit = true
	}
	paused := 0
	for _, sb := range f.items {
		if sb.Status == "paused" {
			paused++
		}
	}
	f.maxPaused = max(f.maxPaused, paused)
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.creates)
	sb := canaryapi.Sandbox{ID: id, Name: req.Name, Metadata: req.Metadata, Status: "active", AccessToken: "token"}
	f.items[id] = sb
	sb.RoutingHint = f.hint(id)
	if f.unknownCreate {
		return canaryapi.Sandbox{}, errors.New("lost create response")
	}
	return sb, nil
}
func (f *residentFake) GetSandbox(_ context.Context, id string) (canaryapi.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.items[id]
	if !ok {
		return sb, canaryapi.ErrNotFound
	}
	sb.RoutingHint = f.hint(id)
	return sb, nil
}
func (f *residentFake) PauseSandbox(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.items[id]
	sb.Status = "paused"
	f.items[id] = sb
	return nil
}
func (f *residentFake) ResumeSandbox(_ context.Context, id string) (canaryapi.ResumeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.items[id]
	sb.Status = "active"
	f.items[id] = sb
	_ = f.clock.wait(context.Background(), f.resumeDuration)
	if f.resumeHost != "" {
		f.host = f.resumeHost
	}
	return canaryapi.ResumeResponse{ID: id, Status: "active", AccessToken: "token", RoutingHint: f.hint(id)}, f.probeError
}
func (f *residentFake) DeleteSandbox(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	delete(f.items, id)
	return nil
}
func (f *residentFake) Exec(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
	return canaryapi.ExecResult{Stdout: "superserve-load-test"}, nil
}
func (f *residentFake) ListSandboxes(_ context.Context, q map[string]string) ([]canaryapi.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupFailure {
		return nil, errors.New("discovery unavailable")
	}
	out := []canaryapi.Sandbox{}
	for _, sb := range f.items {
		match := true
		for key, value := range q {
			if strings.HasPrefix(key, "metadata.") && sb.Metadata[strings.TrimPrefix(key, "metadata.")] != value {
				match = false
			}
		}
		if match {
			out = append(out, sb)
			if len(out) == 2 {
				break
			}
		}
	}
	return out, nil
}

func residentTestConfig(t *testing.T) (Config, *residentClock, *residentFake) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &residentClock{now: now}
	dir := t.TempDir()
	cfg := testRunnerConfig()
	cfg.APIBaseURL = stagingAPIBaseURL
	cfg.PreviewDomain = stagingPreviewDomain
	cfg.Scenario = "resident"
	cfg.LockBackend = "file"
	cfg.LockFile = filepath.Join(dir, "guard.lock")
	cfg.RunTimeout = time.Minute
	cfg.ResourceTTL = 3 * time.Hour
	cfg.Operations = 10
	cfg.Concurrency = 2
	cfg.Resident = ResidentConfig{Population: 4, Stages: []int{2, 4}, Hold: 3 * time.Millisecond, ProbeInterval: time.Millisecond, JournalPath: filepath.Join(dir, "journal.jsonl"), InventoryPath: filepath.Join(dir, "inventory.json"), Mode: "qualify", Preflight: ResidentPreflight{RunID: cfg.RunID, TeamID: "10000000-0000-4000-8000-000000000001", Target: cfg.Target, ExpectedHost: "isolated-host", Template: cfg.Template, ReviewedAt: now, ValidUntil: now.Add(2 * time.Hour), CapacityPopulation: 10, CapacityConcurrency: 2, IsolatedHost: true, QuotaChecked: true, CreditsChecked: true, DiskCapacityChecked: true, RetentionChecked: true, BillingWritesChecked: true, TelemetryChecked: true, ControlPlaneRevision: strings.Repeat("a", 40), HostRevision: strings.Repeat("b", 40), ComparisonRevision: strings.Repeat("c", 40), MaxPhaseLatencyMillis: 1000}}
	f := &residentFake{clock: clock, items: map[string]canaryapi.Sandbox{}, host: "isolated-host"}
	return cfg, clock, f
}
func residentTestRunner(cfg Config, clock *residentClock, f *residentFake) Runner {
	return Runner{Config: cfg, Ops: lifecycle.Operations{Client: f, Clock: clock.read}, Clock: clock.read, Wait: clock.wait, ReadResidentInventory: func(context.Context) (ResidentInventoryEvidence, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ids := []string{}
		paused := 0
		wrongHost := 0
		for _, sb := range f.items {
			slot, _ := strconv.Atoi(sb.Metadata[residentSlotKey])
			if slot >= cfg.Resident.Population {
				continue
			}
			ids = append(ids, sb.ID)
			if sb.Status == "paused" {
				paused++
			}
			if f.host != cfg.Resident.Preflight.ExpectedHost {
				wrongHost++
			}
		}
		digest, err := residentIDsDigest(ids)
		return ResidentInventoryEvidence{Source: "control-plane-sandbox-read", ObservedAt: clock.read(), RunID: cfg.RunID, TeamID: cfg.Resident.Preflight.TeamID, WorkerID: cfg.WorkerID, Target: cfg.Target, Host: cfg.Resident.Preflight.ExpectedHost, Population: len(ids), Paused: paused, WrongHostCount: wrongHost, IDsSHA256: digest}, err
	}}
}

func TestResidentRetainsPopulationAcrossProbesThenCleansOwnedResources(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Resident.Paused != 4 || len(summary.Resident.Stages) != 2 || summary.Resident.Stages[0].Population != 2 || summary.Resident.Stages[1].Population != 4 || summary.Resident.ProbeAttempts != 6 || f.maxPaused != 4 || len(f.items) != 0 || summary.Resident.Remaining != 0 {
		t.Fatalf("population/probes/cleanup: summary=%+v maxPaused=%d items=%d", summary.Resident, f.maxPaused, len(f.items))
	}
	if summary.Resident.Resume.Attempts != 6 || summary.Resident.Create.Attempts != 6 {
		t.Fatalf("missing probe statistics: %+v", summary.Resident)
	}
}

func sharedResidentTestConfig(t *testing.T) (Config, *residentClock, *residentFake) {
	t.Helper()
	cfg, clock, f := residentTestConfig(t)
	cfg.LockBackend = "gcs"
	cfg.LockBucket = sharedResidentLockBucket
	cfg.LockFile = ""
	cfg.LockTTL = 45 * time.Minute
	cfg.BenchmarkGuard, cfg.GlobalLock = true, true
	cfg.Resident.Preflight.IsolatedHost = false
	cfg.Resident.Preflight.SharedHostSmall = true
	return cfg, clock, f
}

func TestResidentSharedSmallCoordinatesAndReportsLimitedScope(t *testing.T) {
	cfg, clock, f := sharedResidentTestConfig(t)
	clock.now = guardTestQuietNow()
	cfg.Resident.Preflight.ReviewedAt = clock.now
	cfg.Resident.Preflight.ValidUntil = clock.now.Add(2 * time.Hour)
	locker := &guardTestLocker{}
	guard, err := AcquireGuard(context.Background(), cfg, locker, clock.read, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release(context.Background())
	want := []string{"loadtest-global", stagingTarget + "-burst", stagingTarget + "-dax", stagingTarget + "-loadtest", stagingTarget + "-soak"}
	if !reflect.DeepEqual(locker.keys, want) {
		t.Fatalf("shared run bypasses existing load/benchmark coordination: %v", locker.keys)
	}
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if err != nil || !summary.Resident.SharedHost || summary.Resident.QualificationScope != "shared-host-small" || summary.Resident.Paused != 4 || len(summary.Resident.Stages) != 2 || len(f.items) != 0 || f.badOrphanLimit {
		t.Fatalf("shared hold/scope/orphan limits/cleanup: summary=%+v err=%v badOrphanLimit=%t", summary.Resident, err, f.badOrphanLimit)
	}
	report := summary.Report(cfg, nil)
	for _, want := range []string{"does not qualify 105000-owner scale", `"shared_host":true`, `"qualification_scope":"shared-host-small"`} {
		if !strings.Contains(report, want) {
			t.Fatalf("shared result missing limitation %q: %s", want, report)
		}
	}
}

func TestResidentSharedSmallAdmissionEnforcesBoundsAndCoordination(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Config)
	}{
		{"population", func(c *Config) { c.Resident.Population = 101; c.Resident.Stages = []int{101} }},
		{"concurrency", func(c *Config) { c.Concurrency = 3 }},
		{"false isolation claim", func(c *Config) { c.Resident.Preflight.IsolatedHost = true }},
		{"retained seed", func(c *Config) { c.Resident.Mode = "seed" }},
		{"no explicit shared proof", func(c *Config) { c.Resident.Preflight.SharedHostSmall = false }},
		{"local lock", func(c *Config) { c.LockBackend = "file"; c.LockFile = filepath.Join(t.TempDir(), "outer.lock") }},
		{"uncoordinated bucket", func(c *Config) { c.LockBucket = "different-staging-bucket" }},
		{"no benchmark guard", func(c *Config) { c.BenchmarkGuard = false }},
		{"no global guard", func(c *Config) { c.GlobalLock = false }},
		{"insufficient capacity", func(c *Config) { c.Resident.Preflight.CapacityPopulation = c.Resident.Population + c.Concurrency - 1 }},
		{"stale capacity", func(c *Config) {
			c.Resident.Preflight.ReviewedAt = c.Resident.Preflight.ReviewedAt.Add(-31 * time.Minute)
		}},
		{"missing inventory", func(c *Config) { c.Resident.InventoryPath = "" }},
		{"production", func(c *Config) { c.Environment = "production"; c.ProductionOptIn = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, clock, _ := sharedResidentTestConfig(t)
			tt.change(&cfg)
			if err := ValidateResidentAdmission(cfg, clock.read()); err == nil {
				t.Fatal("unsafe shared-host run admitted")
			}
		})
	}
	cfg, clock, _ := sharedResidentTestConfig(t)
	cfg.Resident.Population, cfg.Resident.Stages = 100, []int{100}
	cfg.Resident.Preflight.CapacityPopulation = 102
	if err := ValidateResidentAdmission(cfg, clock.read()); err != nil {
		t.Fatalf("bounded shared capacity rejected: %v", err)
	}
}

func TestResidentWrongHostAbortsAndCleansCreatedOwner(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	cfg.Concurrency = 1
	f.host = "another-host"
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if err == nil || f.creates != 1 || len(f.items) != 0 || summary.Resident.Remaining != 0 {
		t.Fatalf("wrong host should stop all dispatch and clean owner: err=%v creates=%d summary=%+v", err, f.creates, summary.Resident)
	}
}

func TestResidentLostCreateResponseDiscoversAndCleansWithoutReplacement(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	cfg.Concurrency = 1
	f.unknownCreate = true
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if err == nil || f.creates != 1 || len(f.items) != 0 || summary.Resident.Remaining != 0 {
		t.Fatalf("unknown create replacement/cleanup: err=%v creates=%d summary=%+v", err, f.creates, summary.Resident)
	}
}

func TestResidentJournalRecoversUnrecordedIDWithoutCreatingReplacement(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	cfg.Resident.Mode = "seed"
	cfg.Resident.Population = 1
	cfg.Resident.Stages = []int{1}
	cfg.Concurrency = 1
	r := residentTestRunner(cfg, clock, f)
	j, err := openResidentJournal(cfg, clock.read())
	if err != nil {
		t.Fatal(err)
	}
	e := residentEntry{Slot: 0, Name: sandboxNameForOperation(cfg.RunID, cfg.WorkerID, 0)}
	if err := j.save(e); err != nil {
		t.Fatal(err)
	}
	metadata := sandboxmetadata.TestOwnershipMetadata(map[string]string{sandboxmetadata.KeyEnvironment: cfg.Environment, sandboxmetadata.KeyCanaryTarget: cfg.Target, residentSlotKey: "0"}, sandboxmetadata.TestOwnership{TestType: sandboxmetadata.TestTypeLoadTest, RunID: cfg.RunID, WorkerID: cfg.WorkerID, CreatedAt: j.header.CreatedAt, ExpiresAt: j.header.ExpiresAt})
	_, err = f.CreateSandbox(context.Background(), canaryapi.CreateSandboxRequest{Name: e.Name, Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	_ = j.close()
	summary, err := r.Run(context.Background())
	if err != nil || f.creates != 1 || summary.Resident.Remaining != 1 {
		t.Fatalf("recover intent: err=%v creates=%d summary=%+v", err, f.creates, summary.Resident)
	}
	cfg.Resident.Mode = "cleanup"
	if _, err := residentTestRunner(cfg, clock, f).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.items) != 0 {
		t.Fatal("cleanup retained owner")
	}
}

func TestResidentUnresolvedIntentBlocksNewSeedAndPreservesJournal(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	cfg.Resident.Mode = "seed"
	j, err := openResidentJournal(cfg, clock.read())
	if err != nil {
		t.Fatal(err)
	}
	if err := j.save(residentEntry{Slot: 0, Name: sandboxNameForOperation(cfg.RunID, cfg.WorkerID, 0)}); err != nil {
		t.Fatal(err)
	}
	_ = j.close()
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if err == nil || f.creates != 0 || summary.Resident.Remaining != 1 || summary.Resident.CleanupFailures != 1 {
		t.Fatalf("unresolved intent must prevent replacement: err=%v creates=%d summary=%+v", err, f.creates, summary.Resident)
	}
}

func TestResidentCleanupRefusesOwnershipChangedAfterSeed(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	cfg.Resident.Mode = "seed"
	if _, err := residentTestRunner(cfg, clock, f).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, sb := range f.items {
		sb.Metadata[sandboxmetadata.KeyRunID] = "unrelated-run"
		f.items[id] = sb
	}
	cfg.Resident.Mode = "cleanup"
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if err == nil || len(f.deleted) != 0 || summary.Resident.Remaining != 4 {
		t.Fatalf("foreign resource mutated: err=%v deleted=%v summary=%+v", err, f.deleted, summary.Resident)
	}
}

func TestResidentProbeFailureAbortsAndCleansEntirePopulation(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	f.probeError = errors.New("resume failed")
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if err == nil || summary.Resident.ProbeFailures != 1 || len(f.items) != 0 {
		t.Fatalf("probe abort cleanup: err=%v summary=%+v", err, summary.Resident)
	}
}

func TestResidentLatencyThresholdAbortsDespiteAllowedProbeFailures(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	cfg.Resident.Preflight.MaxPhaseLatencyMillis = 1
	cfg.Resident.Preflight.MaxProbeFailures = 10
	f.resumeDuration = 2 * time.Millisecond
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if !errors.Is(err, errResidentLatency) || summary.Resident.ProbeAttempts != 1 || summary.Resident.Resume.Failures != 1 || summary.Resident.Resume.P99Millis != 2 || len(f.items) != 0 {
		t.Fatalf("latency abort not enforced: err=%v summary=%+v", err, summary.Resident)
	}
}

func TestResidentResumePlacementMismatchAbortsDespiteFailureBudget(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	cfg.Resident.Preflight.MaxProbeFailures = 10
	f.resumeHost = "other-host"
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if !errors.Is(err, errResidentUnsafe) || summary.Resident.ProbeAttempts != 1 || len(f.items) != 0 {
		t.Fatalf("placement mismatch tolerated: err=%v summary=%+v", err, summary.Resident)
	}
}

func TestResidentAuthoritativeInventoryFailuresPreventQualification(t *testing.T) {
	for _, kind := range []string{"missing", "stale", "wrong-host", "wrong-digest", "vanished-owner"} {
		t.Run(kind, func(t *testing.T) {
			cfg, clock, f := residentTestConfig(t)
			runner := residentTestRunner(cfg, clock, f)
			read := runner.ReadResidentInventory
			runner.ReadResidentInventory = func(ctx context.Context) (ResidentInventoryEvidence, error) {
				if kind == "missing" {
					return ResidentInventoryEvidence{}, os.ErrNotExist
				}
				if kind == "vanished-owner" {
					f.mu.Lock()
					if f.creates > 2 {
						for id, sb := range f.items {
							if sb.Metadata[residentSlotKey] == "0" {
								delete(f.items, id)
								break
							}
						}
					}
					f.mu.Unlock()
				}
				e, err := read(ctx)
				if e.Population >= 2 {
					switch kind {
					case "stale":
						e.ObservedAt = e.ObservedAt.Add(-2 * time.Minute)
					case "wrong-host":
						e.Host = "other-host"
					case "wrong-digest":
						e.IDsSHA256 = strings.Repeat("0", 64)
					}
				}
				return e, err
			}
			summary, err := runner.Run(context.Background())
			if err == nil || len(summary.Resident.Stages) != 0 || len(f.items) != 0 {
				t.Fatalf("false fleet qualification: err=%v summary=%+v remaining=%d", err, summary.Resident, len(f.items))
			}
			if kind == "missing" && f.creates != 0 {
				t.Fatal("created resources before observer was available")
			}
		})
	}
}

func TestResidentAdmissionRejectsJournalLockAliasesWithoutTruncation(t *testing.T) {
	for _, kind := range []string{"same", "cleaned", "symlink", "hardlink", "owner-lock"} {
		t.Run(kind, func(t *testing.T) {
			cfg, clock, _ := residentTestConfig(t)
			original := []byte("durable prior owner journal\n")
			if err := os.WriteFile(cfg.Resident.JournalPath, original, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "same":
				cfg.LockFile = cfg.Resident.JournalPath
			case "cleaned":
				cfg.LockFile = filepath.Dir(cfg.Resident.JournalPath) + "/unused/../" + filepath.Base(cfg.Resident.JournalPath)
			case "symlink":
				cfg.LockFile = filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(cfg.Resident.JournalPath, cfg.LockFile); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				cfg.LockFile = filepath.Join(t.TempDir(), "alias")
				if err := os.Link(cfg.Resident.JournalPath, cfg.LockFile); err != nil {
					t.Fatal(err)
				}
			case "owner-lock":
				cfg.LockFile = cfg.Resident.JournalPath + ".owner-lock"
			}
			if err := ValidateResidentAdmission(cfg, clock.read()); err == nil {
				t.Fatal("state/lock alias accepted")
			}
			after, err := os.ReadFile(cfg.Resident.JournalPath)
			if err != nil || string(after) != string(original) {
				t.Fatalf("journal was changed during admission: %q %v", after, err)
			}
		})
	}
}

func TestResidentCleanupWorksAfterOriginalEvidenceAndRetentionExpire(t *testing.T) {
	cfg, clock, f := residentTestConfig(t)
	cfg.Resident.Mode = "seed"
	if _, err := residentTestRunner(cfg, clock, f).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = clock.wait(context.Background(), 4*time.Hour)
	cfg.Resident.Mode = "cleanup"
	summary, err := residentTestRunner(cfg, clock, f).Run(context.Background())
	if err != nil || summary.Resident.Remaining != 0 || len(f.items) != 0 {
		t.Fatalf("expired cleanup prevented: err=%v summary=%+v", err, summary.Resident)
	}
}

func TestResidentConfigAllowsLargePopulationOnlyWithBoundCapacityEvidence(t *testing.T) {
	setRequiredEnv(t)
	cfg, _, _ := residentTestConfig(t)
	cfg.Resident.Preflight.ReviewedAt = time.Now().UTC()
	cfg.Resident.Preflight.ValidUntil = time.Now().Add(3 * time.Hour)
	cfg.Resident.Preflight.RunID = "explicit-run"
	cfg.Resident.Preflight.Template = "superserve/python-3.11"
	cfg.Resident.Preflight.CapacityPopulation = 210010
	cfg.Resident.Preflight.CapacityConcurrency = 10
	file := filepath.Join(t.TempDir(), "preflight.json")
	data, _ := json.Marshal(cfg.Resident.Preflight)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"LOAD_TEST_SCENARIO": "resident", "LOAD_TEST_RESIDENT_OPT_IN": "true", "LOAD_TEST_RESIDENT_POPULATION": "105000", "LOAD_TEST_RESIDENT_HOLD": "15m", "LOAD_TEST_RESIDENT_MODE": "qualify", "LOAD_TEST_RESIDENT_PREFLIGHT": file, "LOAD_TEST_RESIDENT_INVENTORY_EVIDENCE": filepath.Join(t.TempDir(), "inventory.json"), "LOAD_TEST_RESIDENT_JOURNAL": filepath.Join(t.TempDir(), "run.jsonl"), "LOAD_TEST_RUN_ID": "explicit-run", "CANARY_LOCK_BACKEND": "file", "CANARY_LOCK_FILE": filepath.Join(t.TempDir(), "guard.lock")} {
		t.Setenv(key, value)
	}
	loaded, err := Load()
	if err != nil || loaded.Resident.Population != 105000 {
		t.Fatalf("large resident config rejected: %+v err=%v", loaded.Resident, err)
	}
	t.Setenv("LOAD_TEST_OPERATIONS", "105000")
	if _, err := Load(); err == nil {
		t.Fatal("normal probe operation cap bypassed")
	}
	t.Setenv("LOAD_TEST_OPERATIONS", "100")
	t.Setenv("CANARY_ENVIRONMENT", "production")
	t.Setenv("CANARY_REGION", "us-west2")
	t.Setenv("CANARY_TARGET", "production-us-west2")
	t.Setenv("API_BASE_URL", "https://api-usw.superserve.ai")
	t.Setenv("PREVIEW_DOMAIN", "usw-sandbox.superserve.ai")
	t.Setenv("LOAD_TEST_PRODUCTION_OPT_IN", "true")
	if _, err := Load(); err == nil {
		t.Fatal("resident mode admitted production")
	}
}

func TestResidentJournalBindingAndCorruptionFailBeforeCreating(t *testing.T) {
	for _, kind := range []string{"other-run", "host-scope", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			cfg, clock, f := residentTestConfig(t)
			j, err := openResidentJournal(cfg, clock.read())
			if err != nil {
				t.Fatal(err)
			}
			_ = j.close()
			if kind == "other-run" {
				cfg.RunID = "other-run"
				cfg.Resident.Preflight.RunID = cfg.RunID
			} else if kind == "host-scope" {
				cfg.Resident.Preflight.IsolatedHost = false
				cfg.Resident.Preflight.SharedHostSmall = true
				cfg.LockBackend, cfg.LockBucket, cfg.LockFile = "gcs", sharedResidentLockBucket, ""
				cfg.BenchmarkGuard, cfg.GlobalLock = true, true
			} else {
				file, err := os.OpenFile(cfg.Resident.JournalPath, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = fmt.Fprintln(file, "invalid-json")
				_ = file.Close()
			}
			if _, err := residentTestRunner(cfg, clock, f).Run(context.Background()); err == nil || f.creates != 0 {
				t.Fatalf("journal safety failure: err=%v creates=%d", err, f.creates)
			}
		})
	}
}
