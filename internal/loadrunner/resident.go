package loadrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

const residentSlotKey = "resident_slot"

var errResidentUnsafe = errors.New("resident resource outcome requires recovery")
var errResidentLatency = errors.New("resident latency abort threshold exceeded")

type ResidentLatency struct {
	Attempts  int64   `json:"attempts"`
	Failures  int64   `json:"failures"`
	P50Millis float64 `json:"p50_ms"`
	P95Millis float64 `json:"p95_ms"`
	P99Millis float64 `json:"p99_ms"`
}

type ResidentStage struct {
	Population          int             `json:"population"`
	StartedAt           time.Time       `json:"started_at"`
	CompletedAt         time.Time       `json:"completed_at"`
	ProbeAttempts       int64           `json:"probe_attempts"`
	InventoryObservedAt time.Time       `json:"inventory_observed_at"`
	Create              ResidentLatency `json:"create"`
	Pause               ResidentLatency `json:"pause"`
	Resume              ResidentLatency `json:"resume"`
}

type ResidentSummary struct {
	RunID                    string          `json:"run_id"`
	Host                     string          `json:"expected_host"`
	Mode                     string          `json:"mode"`
	SharedHost               bool            `json:"shared_host"`
	QualificationScope       string          `json:"qualification_scope"`
	Seeded                   int             `json:"seeded"`
	Paused                   int             `json:"paused"`
	Stages                   []ResidentStage `json:"stages"`
	ProbeAttempts            int64           `json:"probe_attempts"`
	ProbeFailures            int64           `json:"probe_failures"`
	CleanupFailures          int64           `json:"cleanup_failures"`
	Remaining                int             `json:"remaining"`
	Duration                 time.Duration   `json:"duration_ns"`
	Error                    string          `json:"error,omitempty"`
	ExternalEvidenceRequired bool            `json:"external_host_database_evidence_required"`
	Create                   ResidentLatency `json:"create"`
	Pause                    ResidentLatency `json:"pause"`
	Resume                   ResidentLatency `json:"resume"`
}

type residentClient interface {
	lifecycle.Client
	ListSandboxes(context.Context, map[string]string) ([]canaryapi.Sandbox, error)
}

func (r Runner) runResident(parent context.Context) (summary Summary, runErr error) {
	cfg := r.Config
	start := r.now()
	ctx, cancel := context.WithTimeout(parent, cfg.RunTimeout)
	defer cancel()
	if err := validateResident(cfg, cfg.Resident, r.now()); err != nil {
		return summary, err
	}
	client, ok := r.Ops.Client.(residentClient)
	if !ok {
		return summary, errors.New("resident client must support bounded ownership discovery")
	}
	if cfg.Resident.JournalPath+".owner-lock" == cfg.LockFile {
		return summary, errors.New("resident journal and outer guard require distinct lock paths")
	}
	outcome, lease, err := lock.NewFileLock(cfg.Resident.JournalPath+".owner-lock").Acquire(ctx, cfg.RunID, cfg.RunTimeout+cleanupTimeout(cfg))
	if err != nil {
		return summary, err
	}
	if outcome != lock.OutcomeAcquired || lease == nil {
		return summary, errors.New("resident journal is already in use")
	}
	defer lease.Release(context.Background())
	j, err := openResidentJournal(cfg, r.now())
	if err != nil {
		return summary, err
	}
	defer j.close()
	scope := "isolated-host"
	if cfg.Resident.Preflight.SharedHostSmall {
		scope = "shared-host-small"
	}
	rs := &ResidentSummary{RunID: cfg.RunID, Host: cfg.Resident.Preflight.ExpectedHost, Mode: cfg.Resident.Mode, SharedHost: cfg.Resident.Preflight.SharedHostSmall, QualificationScope: scope, ExternalEvidenceRequired: true}
	summary = Summary{ConfiguredOperations: int64(cfg.Operations), ConfiguredConcurrency: int64(cfg.Concurrency), FailurePhases: map[string]int64{}, Resident: rs}
	defer func() {
		if runErr != nil || cfg.Resident.Mode != "seed" {
			cleanupCtx, cleanupCancel := residentCleanupContext(ctx, cleanupTimeout(cfg))
			rs.CleanupFailures = r.cleanupResident(cleanupCtx, client, j)
			cleanupCancel()
			if rs.CleanupFailures > 0 {
				runErr = errors.Join(runErr, fmt.Errorf("resident cleanup left %d unresolved resources; journal and janitor ownership retained", rs.CleanupFailures))
			}
			if runErr == nil {
				runErr = j.complete()
			}
		}
		for _, e := range j.snapshot() {
			if !e.Deleted {
				rs.Remaining++
			}
		}
		rs.Duration = r.now().Sub(start)
		summary.Duration = rs.Duration
		summary.CleanupFailures = rs.CleanupFailures
		summary.Started = rs.ProbeAttempts
		summary.Completed = rs.ProbeAttempts - rs.ProbeFailures
		summary.Failed = rs.ProbeFailures
		summary.Unstarted = int64(cfg.Operations) - rs.ProbeAttempts
		if runErr != nil {
			rs.Error = runErr.Error()
		}
		rs.Create = residentLatency(summary.Create)
		rs.Pause = residentLatency(summary.Pause)
		rs.Resume = residentLatency(summary.Resume)
		log.Info().Str("run_id", cfg.RunID).Str("target", cfg.Target).Str("host", rs.Host).Int("resident_remaining", rs.Remaining).Int64("probe_attempts", rs.ProbeAttempts).Int64("cleanup_failures", rs.CleanupFailures).Dur("duration", rs.Duration).Msg("resident storage qualification ended")
	}()
	if cfg.Resident.Mode == "cleanup" {
		return summary, nil
	}
	initialInventory, err := r.residentInventory(ctx)
	if err != nil {
		return summary, err
	}
	if len(j.snapshot()) == 0 && initialInventory.Population != 0 {
		return summary, errors.New("resident inventory already contains run owners absent from the journal; refusing replacement")
	}
	if err := r.recoverResident(ctx, client, j); err != nil {
		return summary, err
	}
	if len(j.snapshot()) == 0 {
		// Establish that the API credential and observer refer to the same
		// team before admitting a large population under that credential.
		if _, err := r.createResident(ctx, client, j, 0, true); err != nil {
			return summary, err
		}
		if _, err := r.waitResidentInventory(ctx, j, 1, r.now()); err != nil {
			return summary, err
		}
	}
	nextProbe := 0
	for _, e := range j.snapshot() {
		if e.Slot >= cfg.Resident.Population {
			nextProbe = max(nextProbe, e.Slot-cfg.Resident.Population+1)
		}
	}
	for _, stage := range cfg.Resident.Stages {
		if err := r.seedResident(ctx, client, j, stage); err != nil {
			return summary, err
		}
		// A resumed setup may already contain a later ramp's population. Do
		// not label measurements of that larger fleet as a smaller stage.
		residentCount := 0
		for _, e := range j.snapshot() {
			if e.Slot < cfg.Resident.Population && !e.Deleted {
				residentCount++
			}
		}
		rs.Seeded = residentCount
		log.Info().Str("run_id", cfg.RunID).Str("host", rs.Host).Int("resident_population", residentCount).Msg("resident seed stage completed; awaiting authoritative inventory")
		if residentCount > stage {
			continue
		}
		proof, err := r.waitResidentInventory(ctx, j, stage, r.now())
		if err != nil {
			return summary, err
		}
		rs.Paused = proof.Paused
		if cfg.Resident.Mode == "seed" {
			continue
		}
		rs.Stages = append(rs.Stages, ResidentStage{Population: stage, StartedAt: r.now().UTC()})
		stageResult := &rs.Stages[len(rs.Stages)-1]
		stageCreate, stagePause, stageResume := summary.Create, summary.Pause, summary.Resume
		stageErr := func() error {
			defer func() {
				stageResult.Create = residentLatencySince(summary.Create, stageCreate)
				stageResult.Pause = residentLatencySince(summary.Pause, stagePause)
				stageResult.Resume = residentLatencySince(summary.Resume, stageResume)
			}()
			stageEvidenceBoundary := proof.ObservedAt
			until := r.now().Add(cfg.Resident.Hold)
			for r.now().Before(until) {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if nextProbe >= cfg.Operations {
					return errors.New("resident probe budget exhausted before hold completed")
				}
				if _, err := r.checkResidentInventory(ctx, j, stage, stageEvidenceBoundary); err != nil {
					return err
				}
				probeErr := r.probeResident(ctx, client, j, &summary, nextProbe)
				nextProbe++
				rs.ProbeAttempts++
				stageResult.ProbeAttempts++
				if probeErr != nil {
					rs.ProbeFailures++
					log.Warn().Err(probeErr).Str("run_id", cfg.RunID).Int("resident_population", stage).Msg("resident foreground probe failed")
				}
				if errors.Is(probeErr, errResidentUnsafe) || errors.Is(probeErr, errResidentLatency) {
					return probeErr
				}
				if rs.ProbeFailures > int64(cfg.Resident.Preflight.MaxProbeFailures) {
					return errors.New("resident probe failure threshold exceeded")
				}
				if err := r.residentWait(ctx, min(cfg.Resident.ProbeInterval, until.Sub(r.now()))); err != nil {
					return err
				}
			}
			stageResult.CompletedAt = r.now().UTC()
			proof, err := r.waitResidentInventory(ctx, j, stage, stageResult.CompletedAt)
			if err != nil {
				return err
			}
			stageResult.InventoryObservedAt = proof.ObservedAt
			return nil
		}()
		if stageErr != nil {
			return summary, stageErr
		}
	}
	return summary, nil
}

func (r Runner) recoverResident(ctx context.Context, c residentClient, j *residentJournal) error {
	entries := j.snapshot()
	sort.Slice(entries, func(i, k int) bool { return entries[i].Slot < entries[k].Slot })
	for _, e := range entries {
		if e.Deleted {
			if e.Slot < r.Config.Resident.Population {
				return errors.New("resident population contains a reclaimed owner; cleanup before replacement")
			}
			continue
		}
		if e.SandboxID == "" {
			var err error
			e, err = r.resolveResidentIntent(ctx, c, j, e)
			if err != nil {
				return err
			}
		}
		sb, err := c.GetSandbox(ctx, e.SandboxID)
		if errors.Is(err, canaryapi.ErrNotFound) {
			return fmt.Errorf("resident owner %d disappeared; cleanup the run before replacement", e.Slot)
		}
		if err != nil {
			return err
		}
		if err := r.checkResidentOwner(sb, e); err != nil {
			return err
		}
		if err := r.checkResidentHost(sb.ID, sb.RoutingHint); err != nil {
			return err
		}
		if e.Slot >= r.Config.Resident.Population {
			if err := r.deleteResident(ctx, c, j, e); err != nil {
				return err
			}
			continue
		}
		if e.Slot < r.Config.Resident.Population && sb.Status != "paused" {
			if sb.Status != "active" && sb.Status != "starting" && sb.Status != "pausing" {
				return fmt.Errorf("resident owner has unsupported recovery status %q", sb.Status)
			}
			if sb.Status != "pausing" {
				if err := r.waitResidentStatus(ctx, c, e, "active"); err != nil {
					return err
				}
				if err := c.PauseSandbox(ctx, e.SandboxID); err != nil {
					return err
				}
			}
			if err := r.waitResidentStatus(ctx, c, e, "paused"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r Runner) resolveResidentIntent(ctx context.Context, c residentClient, j *residentJournal, e residentEntry) (residentEntry, error) {
	q := sandboxmetadata.OwnershipQuery(r.Config.Environment, sandboxmetadata.ManagedByTestRunner)
	q["metadata."+sandboxmetadata.KeyRunID] = r.Config.RunID
	q["metadata."+sandboxmetadata.KeyWorkerID] = r.Config.WorkerID
	q["metadata."+residentSlotKey] = strconv.Itoa(e.Slot)
	q["limit"] = "2"
	q["offset"] = "0"
	items, err := c.ListSandboxes(ctx, q)
	if err != nil {
		return e, err
	}
	if len(items) != 1 {
		return e, fmt.Errorf("resident create outcome for slot %d unresolved (%d matches); refusing replacement", e.Slot, len(items))
	}
	if err := r.checkResidentOwner(items[0], e); err != nil {
		return e, err
	}
	e.SandboxID = items[0].ID
	return e, j.save(e)
}

func (r Runner) seedResident(ctx context.Context, c residentClient, j *residentJournal, population int) error {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var wg sync.WaitGroup
	var first error
	var mu sync.Mutex
	for worker := 0; worker < r.Config.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for slot := range jobs {
				if workCtx.Err() != nil {
					return
				}
				if _, ok := j.entry(slot); ok {
					continue
				}
				if _, err := r.createResident(workCtx, c, j, slot, true); err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
					cancel()
					return
				}
			}
		}()
	}
dispatch:
	for slot := 0; slot < population; slot++ {
		select {
		case <-workCtx.Done():
			break dispatch
		case jobs <- slot:
		}
	}
	close(jobs)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

func (r Runner) createResident(ctx context.Context, c residentClient, j *residentJournal, slot int, pause bool) (residentEntry, error) {
	e := residentEntry{Slot: slot, Name: sandboxNameForOperation(r.Config.RunID, r.Config.WorkerID, slot)}
	if err := j.save(e); err != nil {
		return e, residentJournalError("create intent", err)
	}
	metadata := sandboxmetadata.TestOwnershipMetadata(map[string]string{sandboxmetadata.KeyEnvironment: r.Config.Environment, sandboxmetadata.KeyRegion: r.Config.Region, sandboxmetadata.KeyCanaryTarget: r.Config.Target, residentSlotKey: strconv.Itoa(slot)}, sandboxmetadata.TestOwnership{TestType: sandboxmetadata.TestTypeLoadTest, RunID: r.Config.RunID, WorkerID: r.Config.WorkerID, CreatedAt: j.header.CreatedAt, ExpiresAt: j.header.ExpiresAt})
	ttl := int((j.header.ExpiresAt.Sub(r.now()) + time.Second - 1) / time.Second)
	if ttl <= 0 {
		return e, errors.New("resident retention expired")
	}
	idle := 60
	sb, err := c.CreateSandbox(ctx, canaryapi.CreateSandboxRequest{Name: e.Name, FromTemplate: r.Config.Template, Metadata: metadata, TimeoutSeconds: 60, IdleTimeoutSeconds: &idle, AutoDeleteSeconds: ttl})
	if err != nil {
		if !uncertainCreateOutcome(err) {
			e.Deleted = true
			if saveErr := j.save(e); saveErr != nil {
				return e, errors.Join(err, saveErr)
			}
		}
		return e, fmt.Errorf("resident create outcome requires recovery: %w", err)
	}
	if sb.ID == "" {
		return e, errors.New("resident create returned no ID; refusing replacement")
	}
	e.SandboxID = sb.ID
	if err := j.save(e); err != nil {
		return e, residentJournalError("created ID", err)
	}
	if err := r.checkResidentHost(sb.ID, sb.RoutingHint); err != nil {
		return e, err
	}
	if err := r.waitResidentStatus(ctx, c, e, "active"); err != nil {
		return e, err
	}
	if pause {
		if err := c.PauseSandbox(ctx, e.SandboxID); err != nil {
			return e, err
		}
		if err := r.waitResidentStatus(ctx, c, e, "paused"); err != nil {
			return e, err
		}
	}
	return e, nil
}

func (r Runner) probeResident(ctx context.Context, c residentClient, j *residentJournal, s *Summary, index int) (probeErr error) {
	slot := r.Config.Resident.Population + index
	// A probe intent recovered after process loss must not be replaced under
	// the same slot. Its resource is retained for owned cleanup instead.
	if _, ok := j.entry(slot); ok {
		return errors.New("resident probe slot already exists; use a fresh run after cleanup")
	}
	s.PeakInFlight = 1
	var e residentEntry
	defer func() {
		if e.SandboxID != "" {
			cleanupCtx, cancel := residentCleanupContext(ctx, cleanupTimeout(r.Config))
			defer cancel()
			if err := r.deleteResident(cleanupCtx, c, j, e); err != nil {
				probeErr = errors.Join(probeErr, errResidentUnsafe, err)
			}
		}
	}()
	phase := func(name string, dst *PhaseSummary, fn func() error) error {
		start := r.now()
		dst.Attempts++
		err := fn()
		duration := r.now().Sub(start)
		if err == nil {
			dst.Durations = append(dst.Durations, duration)
		}
		if duration > time.Duration(r.Config.Resident.Preflight.MaxPhaseLatencyMillis)*time.Millisecond {
			err = errors.Join(err, fmt.Errorf("%w: %s", errResidentLatency, name))
		}
		if err != nil {
			dst.Failures++
		}
		r.Ops.RecordStep(ctx, lifecycle.TelemetryContext{Environment: r.Config.Environment, Region: r.Config.Region, Target: r.Config.Target, Scenario: "loadtest-resident"}, name, stepOutcome(err), duration)
		return err
	}
	if err := phase("resident_probe_create", &s.Create, func() error { var err error; e, err = r.createResident(ctx, c, j, slot, false); return err }); err != nil {
		return errors.Join(errResidentUnsafe, err)
	}
	if err := phase("resident_probe_pause", &s.Pause, func() error {
		if err := c.PauseSandbox(ctx, e.SandboxID); err != nil {
			return err
		}
		return r.waitResidentStatus(ctx, c, e, "paused")
	}); err != nil {
		return err
	}
	if err := phase("resident_probe_resume", &s.Resume, func() error {
		resp, err := c.ResumeSandbox(ctx, e.SandboxID)
		if err != nil {
			return err
		}
		if err := r.checkResidentHost(e.SandboxID, resp.RoutingHint); err != nil {
			return err
		}
		return r.waitResidentStatus(ctx, c, e, "active")
	}); err != nil {
		return err
	}
	sb, err := c.GetSandbox(ctx, e.SandboxID)
	if err != nil {
		return err
	}
	telemetry := lifecycle.TelemetryContext{Environment: r.Config.Environment, Region: r.Config.Region, Target: r.Config.Target, Scenario: "loadtest-resident"}
	if err := r.verifyExec(ctx, e.SandboxID, sb.AccessToken, "resident_probe_exec", telemetry); err != nil {
		return err
	}
	return nil
}

func (r Runner) waitResidentStatus(ctx context.Context, c residentClient, e residentEntry, want string) error {
	waitCtx, cancel := context.WithTimeout(ctx, r.Config.ActiveTimeout)
	defer cancel()
	for {
		sb, err := c.GetSandbox(waitCtx, e.SandboxID)
		if err != nil {
			return err
		}
		if err := r.checkResidentOwner(sb, e); err != nil {
			return err
		}
		if err := r.checkResidentHost(sb.ID, sb.RoutingHint); err != nil {
			return err
		}
		if sb.Status == want {
			return nil
		}
		if sb.Status == "failed" || sb.Status == "deleted" {
			return fmt.Errorf("resident owner entered %s", sb.Status)
		}
		if err := r.residentWait(waitCtx, r.Config.PollInterval); err != nil {
			return err
		}
	}
}

func (r Runner) checkResidentOwner(sb canaryapi.Sandbox, e residentEntry) error {
	m := sb.Metadata
	if sb.ID == "" || e.SandboxID != "" && sb.ID != e.SandboxID || sb.Name != e.Name || m[sandboxmetadata.KeyManagedBy] != sandboxmetadata.ManagedByTestRunner || m[sandboxmetadata.KeyEnvironment] != r.Config.Environment || m[sandboxmetadata.KeyCanaryTarget] != r.Config.Target || m[sandboxmetadata.KeyTestType] != sandboxmetadata.TestTypeLoadTest || m[sandboxmetadata.KeyRunID] != r.Config.RunID || m[sandboxmetadata.KeyWorkerID] != r.Config.WorkerID || m[residentSlotKey] != strconv.Itoa(e.Slot) {
		return errors.New("resident resource ownership mismatch; refusing mutation")
	}
	if _, ok := sandboxmetadata.MatchOwnership(m, r.Config.Environment); !ok {
		return errors.New("resident resource lacks valid expiry ownership")
	}
	return nil
}

func (r Runner) checkResidentHost(id, token string) error {
	// This is placement evidence from the authenticated API response, not
	// capability validation: the runner never receives the server's HMAC key.
	parts := strings.Split(token, ".")
	if len(token) > 2048 || len(parts) != 3 || parts[0] != "v1" || parts[2] == "" {
		return errors.Join(errResidentUnsafe, errors.New("resident placement evidence missing or malformed"))
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.Join(errResidentUnsafe, errors.New("resident placement evidence malformed"))
	}
	var h struct {
		Sandbox  string `json:"s"`
		Host     string `json:"h"`
		Audience string `json:"a"`
		Expires  int64  `json:"e"`
		Version  int64  `json:"v"`
	}
	if json.Unmarshal(data, &h) != nil || h.Host != r.Config.Resident.Preflight.ExpectedHost || h.Audience != r.Config.PreviewDomain || h.Expires <= r.now().Unix() || h.Expires > r.now().Add(61*time.Minute).Unix() || h.Version <= 0 || !(id == h.Sandbox || strings.HasSuffix(id, "-"+h.Sandbox)) {
		return errors.Join(errResidentUnsafe, errors.New("resident placement does not match expected host, sandbox or fresh audience"))
	}
	return nil
}

func (r Runner) cleanupResident(ctx context.Context, c residentClient, j *residentJournal) int64 {
	var failures int64
	jobs := make(chan residentEntry)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for worker := 0; worker < r.Config.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				if e.Deleted {
					continue
				}
				ownerCtx, ownerCancel := context.WithTimeout(ctx, r.Config.DeleteTimeout)
				err := ownerCtx.Err()
				if err == nil && e.SandboxID == "" {
					e, err = r.resolveResidentIntent(ownerCtx, c, j, e)
				}
				if err == nil {
					err = r.deleteResident(ownerCtx, c, j, e)
				}
				ownerCancel()
				if err != nil {
					mu.Lock()
					failures++
					mu.Unlock()
				}
			}
		}()
	}
	for _, e := range j.snapshot() {
		jobs <- e
	}
	close(jobs)
	wg.Wait()
	return failures
}

// Probe and final fleet cleanup share the run's latest cleanup deadline. A
// failed probe near timeout must not consume a second lease-sized grace period.
func residentCleanupContext(runCtx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if runDeadline, ok := runCtx.Deadline(); ok && runDeadline.Add(timeout).Before(deadline) {
		deadline = runDeadline.Add(timeout)
	}
	return context.WithDeadline(context.WithoutCancel(runCtx), deadline)
}

func residentLatencySince(current, before PhaseSummary) ResidentLatency {
	return residentLatency(PhaseSummary{
		Attempts:  current.Attempts - before.Attempts,
		Failures:  current.Failures - before.Failures,
		Durations: current.Durations[len(before.Durations):],
	})
}

func residentLatency(p PhaseSummary) ResidentLatency {
	out := ResidentLatency{Attempts: p.Attempts, Failures: p.Failures}
	if len(p.Durations) == 0 {
		return out
	}
	ordered := append([]time.Duration(nil), p.Durations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	out.P50Millis = float64(nearestRank(ordered, 50)) / float64(time.Millisecond)
	out.P95Millis = float64(nearestRank(ordered, 95)) / float64(time.Millisecond)
	out.P99Millis = float64(nearestRank(ordered, 99)) / float64(time.Millisecond)
	return out
}

func (r Runner) deleteResident(ctx context.Context, c residentClient, j *residentJournal, e residentEntry) error {
	ctx, cancel := context.WithTimeout(ctx, r.Config.DeleteTimeout)
	defer cancel()
	sb, err := c.GetSandbox(ctx, e.SandboxID)
	if errors.Is(err, canaryapi.ErrNotFound) || err == nil && sb.Status == "deleted" {
		e.Deleted = true
		return j.save(e)
	}
	if err != nil {
		return err
	}
	if err := r.checkResidentOwner(sb, e); err != nil {
		return err
	}
	if err := c.DeleteSandbox(ctx, e.SandboxID); err != nil && !errors.Is(err, canaryapi.ErrNotFound) {
		return err
	}
	if err := r.waitDeleted(ctx, e.SandboxID); err != nil {
		return err
	}
	e.Deleted = true
	return j.save(e)
}

func (r Runner) residentWait(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if d <= 0 {
		return nil
	}
	if r.Wait != nil {
		return r.Wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
