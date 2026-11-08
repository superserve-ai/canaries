package loadrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

const launchScenarioLabel = "loadtest-launch"

// Failure classes keep generator, API, quota, capacity and host problems
// apart in the report.
const (
	FailureRateLimited  = "rate_limited"
	FailureTeamQuota    = "team_quota"
	FailureHostCap      = "host_cap"
	FailureServerError  = "server_error"
	FailureRejected     = "rejected"
	FailureTransport    = "transport"
	FailureCanceled     = "canceled"
	FailureActiveFailed = "active_failed"
	FailureActiveWait   = "active_timeout"
	FailureFirstCommand = "first_command"
)

var failureMilestones = []int64{100, 1_000, 10_000, 100_000, 1_000_000}

var errRequestNotSent = errors.New("request not sent")

// TemplateInfo is the template size recorded with each launch report.
type TemplateInfo struct {
	Name      string
	Known     bool
	VCPU      int
	MemoryMiB int
	DiskMiB   int
	SizeBytes int64
}

type LaunchSecond struct {
	Accepted int64
	Ready    int64
	Failed   int64
}

type FailureMilestone struct {
	Operations int64
	Failed     int64
}

type CleanupSummary struct {
	Resident       int64
	Deleted        int64
	DeleteFailures int64
	Swept          int64
	Leftover       int64
	Verified       bool
	Note           string
}

// LaunchSummary holds one generator task's observations. Rates and
// percentiles are task-local; a fan-out run sums task reports.
type LaunchSummary struct {
	Configured            int64
	TaskOperations        int64
	ConfiguredConcurrency int64
	Dispatched            int64
	Created               int64
	Active                int64
	Ready                 int64
	Failed                int64
	Unstarted             int64
	UncertainCreates      int64
	PeakInFlight          int64
	Start                 time.Time
	LaunchEnd             time.Time
	End                   time.Time
	HoldDuration          time.Duration
	TeardownDuration      time.Duration
	CreateRequest         []time.Duration
	CreateToActive        []time.Duration
	TimeToFirstCommand    []time.Duration
	ScheduleLag           []time.Duration
	SlotWait              []time.Duration
	APIWait               time.Duration
	APIWaitCalls          int64
	Timeline              []LaunchSecond
	FailureClasses        map[string]int64
	FailureMilestones     []FailureMilestone
	Template              TemplateInfo
	Cleanup               CleanupSummary
	Pressure              PressureReport
	StopReason            string
}

type sandboxLister interface {
	ListSandboxes(context.Context, map[string]string) ([]canaryapi.Sandbox, error)
}

// launchAPIClient spends this task's share of the team API budget on
// control-plane calls. Exec goes through the sandbox proxy, which the API
// rate limit does not cover, so it stays unthrottled.
type launchAPIClient struct {
	lifecycle.Client
	pacer *pacer
}

func (c *launchAPIClient) Wait(ctx context.Context) error {
	return c.pacer.wait(ctx)
}

func (c *launchAPIClient) GetSandbox(ctx context.Context, id string) (canaryapi.Sandbox, error) {
	if err := c.Wait(ctx); err != nil {
		return canaryapi.Sandbox{}, err
	}
	return c.Client.GetSandbox(ctx, id)
}

func (c *launchAPIClient) DeleteSandbox(ctx context.Context, id string) error {
	if err := c.Wait(ctx); err != nil {
		return err
	}
	return c.Client.DeleteSandbox(ctx, id)
}

func (c *launchAPIClient) ListSandboxes(ctx context.Context, query map[string]string) ([]canaryapi.Sandbox, error) {
	lister, ok := c.Client.(sandboxLister)
	if !ok {
		return nil, errors.New("API client cannot list sandboxes")
	}
	if err := c.Wait(ctx); err != nil {
		return nil, err
	}
	return lister.ListSandboxes(ctx, query)
}

// pacer hands out evenly spaced request slots without bursts.
type pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
	waited   atomic.Int64
	calls    atomic.Int64
}

func newPacer(rate float64) *pacer {
	return &pacer{interval: rateInterval(rate)}
}

// rateInterval is the spacing between events at rate per second. It
// saturates at an hour so a tiny rate cannot overflow into a negative
// duration, which would disable pacing.
func rateInterval(rate float64) time.Duration {
	if interval := float64(time.Second) / rate; interval < float64(time.Hour) {
		return time.Duration(interval)
	}
	return time.Hour
}

func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	slot := p.next
	if slot.Before(now) {
		slot = now
	}
	p.next = slot.Add(p.interval)
	p.mu.Unlock()
	delay := slot.Sub(now)
	p.calls.Add(1)
	if delay <= 0 {
		return nil
	}
	p.waited.Add(int64(delay))
	if err := sleepContext(ctx, delay); err != nil {
		return fmt.Errorf("%w: %w", errRequestNotSent, err)
	}
	return nil
}

// dropReservations forgets slots handed to callers that gave up before
// sending, so cleanup does not queue behind requests that never happen. Call
// it only when no caller is waiting.
func (p *pacer) dropReservations() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit := time.Now().Add(p.interval); p.next.After(limit) {
		p.next = limit
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
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

type launchResult struct {
	seq         int64
	operation   int
	sandboxID   string
	createdAt   time.Time
	createReq   time.Duration
	toActive    time.Duration
	toCommand   time.Duration
	active      bool
	ready       bool
	failure     string
	uncertain   bool
	err         error
	completedAt time.Time
}

type launchState struct {
	mu       sync.Mutex
	summary  LaunchSummary
	resident []string
}

func (s *launchState) bucket(sec int) *LaunchSecond {
	sec = max(sec, 0)
	for len(s.summary.Timeline) <= sec {
		s.summary.Timeline = append(s.summary.Timeline, LaunchSecond{})
	}
	return &s.summary.Timeline[sec]
}

// RunLaunch creates this task's share of the configured sandboxes at the
// configured rate, keeps them resident for the hold period, then deletes
// and verifies every sandbox the run owns. monitor may be nil.
func (r Runner) RunLaunch(parent context.Context, monitor *PressureMonitor) (LaunchSummary, error) {
	cfg := r.Config
	share := cfg.Launch.TaskOperations(cfg.Operations)
	state := &launchState{summary: LaunchSummary{
		Configured: int64(cfg.Operations), TaskOperations: int64(share), ConfiguredConcurrency: int64(cfg.Concurrency),
		FailureClasses: make(map[string]int64), Template: TemplateInfo{Name: cfg.Template},
	}}
	if share <= 0 || share > MaxOperations || cfg.Concurrency <= 0 || cfg.Concurrency > share || cfg.Launch.APIRate <= 0 {
		state.summary.Unstarted = int64(share)
		return state.summary, errors.New("launch requires a positive task share of at most MaxOperations, concurrency within the share, and a positive API rate")
	}
	api := &launchAPIClient{Client: r.Ops.Client, pacer: newPacer(cfg.Launch.TaskRate(cfg.Launch.APIRate))}
	r.Ops.Client = api
	telemetry := lifecycle.TelemetryContext{Environment: cfg.Environment, Region: cfg.Region, Target: cfg.Target, Scenario: launchScenarioLabel}

	// The lease budget covers RunTimeout, so the template lookup runs inside it.
	timeoutCtx, timeoutCancel := context.WithTimeout(parent, cfg.RunTimeout)
	defer timeoutCancel()
	state.summary.Template = r.lookupTemplate(timeoutCtx, api)
	ctx, cancel := context.WithCancel(timeoutCtx)
	defer cancel()
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	defer stopMonitor()
	var monitorDone sync.WaitGroup
	if monitor != nil {
		// Read host state before the first create; an abort-level reading
		// launches nothing and a backoff pauses the first Gate.
		monitor.Evaluate(ctx)
		monitorDone.Add(1)
		go func() {
			defer monitorDone.Done()
			// Run returns as soon as the guard aborts; stop in-flight work too.
			monitor.Run(monitorCtx)
			if monitor.Err() != nil {
				cancel()
			}
		}()
	}

	start := r.now()
	state.summary.Start = start.UTC()
	r.dispatchLaunch(ctx, state, monitor, api, telemetry, share, start)
	state.summary.LaunchEnd = r.now().UTC()

	if cfg.Launch.Hold > 0 && ctx.Err() == nil {
		holdStart := r.now()
		log.Info().Str("run_id", cfg.RunID).Int64("ready", state.summary.Ready).Dur("hold", cfg.Launch.Hold).Msg("launch holding sandboxes resident")
		_ = sleepContext(ctx, cfg.Launch.Hold)
		state.summary.HoldDuration = r.now().Sub(holdStart)
	}
	stopMonitor()
	monitorDone.Wait()
	// Teardown has its own budget; a deadline it crosses is not a workload
	// timeout.
	workloadErr, parentErr := timeoutCtx.Err(), parent.Err()

	switch {
	case monitor.Err() != nil:
		state.summary.StopReason = monitor.Err().Error()
	case errors.Is(workloadErr, context.DeadlineExceeded) && parentErr == nil:
		state.summary.StopReason = "run timeout"
	case parentErr != nil:
		state.summary.StopReason = "canceled"
	}

	api.pacer.dropReservations()
	teardownStart := r.now()
	state.summary.Cleanup = r.teardownLaunch(state, api, telemetry)
	state.summary.TeardownDuration = r.now().Sub(teardownStart)
	state.summary.End = r.now().UTC()
	state.summary.APIWait = time.Duration(api.pacer.waited.Load())
	state.summary.APIWaitCalls = api.pacer.calls.Load()
	state.summary.Pressure = monitor.Report()
	state.summary.Unstarted = int64(share) - state.summary.Dispatched

	summary := state.summary
	log.Info().Str("run_id", cfg.RunID).Str("worker_id", cfg.WorkerID).Str("environment", cfg.Environment).Str("target", cfg.Target).
		Int64("task_operations", summary.TaskOperations).Int64("dispatched", summary.Dispatched).Int64("created", summary.Created).
		Int64("ready", summary.Ready).Int64("failed", summary.Failed).Int64("unstarted", summary.Unstarted).
		Int64("peak_in_flight", summary.PeakInFlight).Int64("cleanup_leftover", summary.Cleanup.Leftover).
		Bool("cleanup_verified", summary.Cleanup.Verified).Str("stop_reason", summary.StopReason).Msg("launch run complete")

	var runErr error
	if summary.Failed > 0 || summary.Unstarted > 0 {
		runErr = fmt.Errorf("%d of %d dispatched launches failed; %d unstarted", summary.Failed, summary.Dispatched, summary.Unstarted)
	}
	if err := monitor.Err(); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if parentErr != nil {
		runErr = errors.Join(runErr, parentErr)
	} else if workloadErr != nil {
		runErr = errors.Join(runErr, workloadErr)
	}
	if !summary.Cleanup.Verified {
		runErr = errors.Join(runErr, fmt.Errorf("launch cleanup not verified: %d sandboxes may remain", summary.Cleanup.Leftover))
	}
	return summary, runErr
}

// dispatchLaunch paces creates and owns the in-flight count. Workers report
// completion over done, so only this goroutine changes inFlight.
func (r Runner) dispatchLaunch(ctx context.Context, state *launchState, monitor *PressureMonitor, api *launchAPIClient, telemetry lifecycle.TelemetryContext, share int, start time.Time) {
	cfg := r.Config
	done := make(chan launchResult, cfg.Concurrency)
	var wg sync.WaitGroup
	inFlight := 0
	collect := func(result launchResult) {
		inFlight--
		r.recordLaunchResult(state, result, start)
	}
	var next time.Time
	for seq := 0; seq < share; seq++ {
		multiplier, paused, err := monitor.Gate(ctx)
		if err != nil || ctx.Err() != nil {
			break
		}
		if paused {
			next = time.Time{}
		}
		scheduled := r.now()
		if rate := cfg.Launch.TaskRate(r.launchRate(scheduled.Sub(start))) * multiplier; rate > 0 {
			now := r.now()
			// Do not burst to catch up more than one second of backlog.
			if next.IsZero() || now.Sub(next) > time.Second {
				next = now
			}
			scheduled = next
			if err := sleepContext(ctx, next.Sub(now)); err != nil {
				break
			}
			next = next.Add(rateInterval(rate))
		}
		limit := max(1, int(math.Ceil(float64(cfg.Concurrency)*multiplier)))
		slotStart := r.now()
	waitSlot:
		for inFlight >= limit {
			select {
			case result := <-done:
				collect(result)
			case <-ctx.Done():
				break waitSlot
			}
		}
		if ctx.Err() != nil {
			break
		}
		slotWait := r.now().Sub(slotStart)
		// Take the API slot here rather than in the worker: a worker queued
		// on the budget would send at the pre-backoff pace, while the next
		// dispatch already sees the guard's current rate and concurrency.
		if err := api.Wait(ctx); err != nil {
			break
		}
		if _, paused, err := monitor.Gate(ctx); err != nil {
			break
		} else if paused {
			// Redo this launch's admission at the post-pause rate.
			next = time.Time{}
			seq--
			continue
		}
		for drained := false; !drained; {
			select {
			case result := <-done:
				collect(result)
			default:
				drained = true
			}
		}
		issued := r.now()
		state.mu.Lock()
		state.summary.Dispatched++
		state.summary.ScheduleLag = append(state.summary.ScheduleLag, max(0, issued.Sub(scheduled)))
		state.summary.SlotWait = append(state.summary.SlotWait, slotWait)
		inFlight++
		state.summary.PeakInFlight = max(state.summary.PeakInFlight, int64(inFlight))
		state.mu.Unlock()
		operation := seq*cfg.Launch.TaskCount + cfg.Launch.TaskIndex
		wg.Add(1)
		go func(seq int64, operation int) {
			defer wg.Done()
			done <- r.launchOne(ctx, monitor, telemetry, seq, operation)
		}(int64(seq), operation)
	}
	go func() { wg.Wait(); close(done) }()
	for result := range done {
		collect(result)
	}
}

// launchRate returns the aggregate target rate at elapsed; zero means the
// launch is bound by concurrency only.
func (r Runner) launchRate(elapsed time.Duration) float64 {
	l := r.Config.Launch
	if l.Strategy != "ramp" {
		return l.Rate
	}
	steps := math.Floor(float64(elapsed) / float64(l.RampStepDuration))
	return math.Min(l.Rate, l.RampStartRate+l.RampStepRate*steps)
}

func (r Runner) launchOne(ctx context.Context, monitor *PressureMonitor, telemetry lifecycle.TelemetryContext, seq int64, operation int) (result launchResult) {
	cfg := r.Config
	result.seq = seq
	result.operation = operation
	defer func() { result.completedAt = r.now() }()
	createdAt := r.now().UTC()
	result.createdAt = createdAt
	metadata := sandboxmetadata.TestOwnershipMetadata(map[string]string{
		sandboxmetadata.KeyEnvironment: cfg.Environment,
		sandboxmetadata.KeyRegion:      cfg.Region,
	}, sandboxmetadata.TestOwnership{TestType: sandboxmetadata.TestTypeLoadTest, RunID: cfg.RunID, WorkerID: cfg.WorkerID, CreatedAt: createdAt, ExpiresAt: createdAt.Add(cfg.ResourceTTL)})
	ttlSeconds := int((cfg.ResourceTTL + time.Second - 1) / time.Second)
	request := canaryapi.CreateSandboxRequest{Name: sandboxNameForOperation(cfg.RunID, cfg.WorkerID, operation), FromTemplate: cfg.Template, TimeoutSeconds: ttlSeconds, AutoDeleteSeconds: ttlSeconds, Metadata: metadata}
	createStart := r.now()
	sb, err := r.Ops.CreateSandbox(ctx, lifecycle.CreateSandboxOptions{Request: request, Telemetry: telemetry})
	result.createReq = r.now().Sub(createStart)
	monitor.ObserveCreate(r.now(), result.createReq, err != nil || sb.ID == "")
	if err != nil {
		result.uncertain = uncertainCreateOutcome(err)
		result.failure = classifyCreateFailure(ctx, err, monitor.AllHostsFull())
		result.err = err
		return
	}
	if sb.ID == "" {
		result.uncertain = true
		result.failure, result.err = FailureServerError, errors.New("create response did not contain a sandbox ID")
		return
	}
	result.sandboxID = sb.ID
	// POST /sandboxes restores the VM before it answers, so an active
	// response needs no polling.
	if sb.Status != "active" {
		if err := r.waitStatus(ctx, sb.ID, "active", "wait_active", telemetry); err != nil {
			result.failure, result.err = classifyActiveFailure(ctx, err), err
			return
		}
	}
	result.active = true
	result.toActive = r.now().Sub(createStart)
	if err := r.verifyExec(ctx, sb.ID, sb.AccessToken, "verify_exec", telemetry); err != nil {
		result.failure, result.err = FailureFirstCommand, err
		if ctx.Err() != nil {
			result.failure = FailureCanceled
		}
		return
	}
	result.ready = true
	result.toCommand = r.now().Sub(createStart)
	r.Ops.RecordStep(ctx, telemetry, "time_to_first_command", "success", result.toCommand)
	return
}

func (r Runner) recordLaunchResult(state *launchState, result launchResult, start time.Time) {
	state.mu.Lock()
	defer state.mu.Unlock()
	s := &state.summary
	if result.sandboxID != "" {
		state.resident = append(state.resident, result.sandboxID)
		s.Created++
		s.CreateRequest = append(s.CreateRequest, result.createReq)
		state.bucket(int(result.createdAt.Add(result.createReq).Sub(start)/time.Second)).Accepted++
	}
	if result.active {
		s.Active++
		s.CreateToActive = append(s.CreateToActive, result.toActive)
	}
	if result.ready {
		s.Ready++
		s.TimeToFirstCommand = append(s.TimeToFirstCommand, result.toCommand)
		state.bucket(int(result.completedAt.Sub(start)/time.Second)).Ready++
	}
	if result.uncertain {
		s.UncertainCreates++
	}
	if result.failure != "" {
		s.Failed++
		s.FailureClasses[result.failure]++
		state.bucket(int(result.completedAt.Sub(start)/time.Second)).Failed++
		for _, milestone := range failureMilestones {
			if int64(result.operation) < milestone {
				s.addMilestoneFailure(milestone)
				break
			}
		}
		log.Warn().Err(result.err).Int64("seq", result.seq).Str("sandbox_id", result.sandboxID).Str("failure_class", result.failure).
			Bool("create_outcome_unknown", result.uncertain).Str("run_id", r.Config.RunID).Str("worker_id", r.Config.WorkerID).Msg("launch failed")
	}
}

// addMilestoneFailure counts a failure in the first milestone that covers
// its global operation index, so fanned-out tasks report the same scale; the report turns these into cumulative counts.
func (s *LaunchSummary) addMilestoneFailure(milestone int64) {
	for i := range s.FailureMilestones {
		if s.FailureMilestones[i].Operations == milestone {
			s.FailureMilestones[i].Failed++
			return
		}
	}
	s.FailureMilestones = append(s.FailureMilestones, FailureMilestone{Operations: milestone, Failed: 1})
}

// classifyCreateFailure separates generator-side throttling from quota,
// placement capacity and platform errors.
func classifyCreateFailure(ctx context.Context, err error, allHostsFull bool) string {
	var statusErr *canaryapi.HTTPStatusError
	if !errors.As(err, &statusErr) {
		if errors.Is(err, errRequestNotSent) || ctx.Err() != nil {
			return FailureCanceled
		}
		// The client turns a 404 into ErrNotFound, not a status error.
		if errors.Is(err, canaryapi.ErrNotFound) {
			return FailureRejected
		}
		return FailureTransport
	}
	code, message := apiErrorFields(statusErr.Body)
	switch {
	case statusErr.StatusCode == http.StatusTooManyRequests && code == "too_many_sandboxes":
		return FailureTeamQuota
	case statusErr.StatusCode == http.StatusTooManyRequests:
		return FailureRateLimited
	case statusErr.StatusCode >= http.StatusInternalServerError:
		lower := strings.ToLower(message + " " + statusErr.Body)
		if strings.Contains(lower, "no hosts available") || strings.Contains(lower, "no available network slots") || allHostsFull {
			return FailureHostCap
		}
		return FailureServerError
	case statusErr.StatusCode == http.StatusRequestTimeout:
		return FailureTransport
	default:
		return FailureRejected
	}
}

func classifyActiveFailure(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return FailureCanceled
	case strings.Contains(err.Error(), "terminal state"):
		return FailureActiveFailed
	default:
		return FailureActiveWait
	}
}

func apiErrorFields(body string) (code, message string) {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil {
		return "", ""
	}
	return payload.Error.Code, payload.Error.Message
}

// lookupTemplate records the template size. A failed lookup only leaves the
// size unknown in the report.
func (r Runner) lookupTemplate(ctx context.Context, api *launchAPIClient) TemplateInfo {
	info := TemplateInfo{Name: r.Config.Template}
	lookupCtx, cancel := context.WithTimeout(ctx, r.Config.HTTPTimeout)
	defer cancel()
	if err := api.Wait(lookupCtx); err != nil {
		return info
	}
	templates, err := api.ListTemplates(lookupCtx, map[string]string{"owner": "all", "name_prefix": r.Config.Template})
	if err != nil {
		log.Warn().Err(err).Str("template", r.Config.Template).Msg("launch template size lookup failed")
		return info
	}
	for _, template := range templates {
		if template.Name == r.Config.Template || template.ID == r.Config.Template {
			return TemplateInfo{Name: r.Config.Template, Known: true, VCPU: template.VCPU, MemoryMiB: template.MemoryMiB, DiskMiB: template.DiskMiB, SizeBytes: template.SizeBytes}
		}
	}
	return info
}

// teardownLaunch deletes every sandbox this task saw created, then sweeps
// the run's ownership metadata for sandboxes whose create outcome was lost.
// It runs on a fresh context so a canceled or aborted launch still cleans up.
func (r Runner) teardownLaunch(state *launchState, api *launchAPIClient, telemetry lifecycle.TelemetryContext) CleanupSummary {
	cfg := r.Config
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Launch.TeardownTimeout)
	defer cancel()
	state.mu.Lock()
	resident := append([]string(nil), state.resident...)
	state.mu.Unlock()
	cleanup := CleanupSummary{Resident: int64(len(resident))}
	deleted, failed := r.deleteAll(ctx, resident, telemetry)
	cleanup.Deleted, cleanup.DeleteFailures = deleted, failed

	query := map[string]string{
		"metadata." + sandboxmetadata.KeyManagedBy:   sandboxmetadata.ManagedByTestRunner,
		"metadata." + sandboxmetadata.KeyEnvironment: cfg.Environment,
		"metadata." + sandboxmetadata.KeyTestType:    sandboxmetadata.TestTypeLoadTest,
		"metadata." + sandboxmetadata.KeyRunID:       cfg.RunID,
		"metadata." + sandboxmetadata.KeyWorkerID:    cfg.WorkerID,
	}
	if _, ok := api.Client.(sandboxLister); !ok {
		cleanup.Leftover = failed
		cleanup.Note = "API client cannot list sandboxes; sweep skipped"
		return cleanup
	}
	pollInterval := max(cfg.PollInterval, 500*time.Millisecond)
	// A create whose response was lost can still land after the client gave
	// up, so an empty sweep only verifies cleanup once the server has had
	// CreateSettle to finish such creates. Verification trusts that bound;
	// the sandbox TTL is the backstop if the server exceeds it.
	state.mu.Lock()
	uncertain := state.summary.UncertainCreates
	state.mu.Unlock()
	var settleUntil time.Time
	if uncertain > 0 {
		settleUntil = time.Now().Add(max(cfg.HTTPTimeout, cfg.Launch.CreateSettle))
	}
	for pass := 0; ; pass++ {
		listStart := time.Now()
		remaining, err := api.ListSandboxes(ctx, query)
		if err != nil {
			cleanup.Leftover = max(cleanup.Leftover, failed)
			cleanup.Note = "sweep list failed: " + err.Error()
			return cleanup
		}
		var ids []string
		for _, sb := range remaining {
			if sb.Status != "deleted" && sb.Metadata[sandboxmetadata.KeyRunID] == cfg.RunID {
				ids = append(ids, sb.ID)
			}
		}
		cleanup.Leftover = int64(len(ids))
		// Only a list issued after the settle window proves a late create
		// is not about to appear.
		if len(ids) == 0 && !listStart.Before(settleUntil) {
			cleanup.Verified = true
			return cleanup
		}
		if ctx.Err() != nil {
			cleanup.Note = "teardown budget exhausted; the janitor reclaims sandboxes after expiry"
			if len(ids) == 0 {
				cleanup.Note = fmt.Sprintf("teardown budget ended before %d uncertain creates settled; the janitor reclaims sandboxes after expiry", uncertain)
			}
			return cleanup
		}
		// Deletes are asynchronous; give the first deletes a moment to
		// settle before sweeping what remains.
		if pass > 0 && len(ids) > 0 {
			swept, _ := r.deleteAll(ctx, ids, telemetry)
			cleanup.Swept += swept
		}
		if sleepContext(ctx, pollInterval) != nil {
			cleanup.Note = "teardown budget exhausted; the janitor reclaims sandboxes after expiry"
			return cleanup
		}
	}
}

func (r Runner) deleteAll(ctx context.Context, ids []string, telemetry lifecycle.TelemetryContext) (deleted, failed int64) {
	jobs := make(chan string)
	var wg sync.WaitGroup
	var deletedCount, failedCount atomic.Int64
	for range min(r.Config.Launch.TeardownConcurrency, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if err := r.Ops.DeleteSandboxBestEffort(ctx, id, lifecycle.DeleteSandboxOptions{Timeout: r.Config.DeleteTimeout, Telemetry: telemetry}); err != nil {
					failedCount.Add(1)
					log.Error().Err(err).Str("sandbox_id", id).Str("run_id", r.Config.RunID).Msg("launch teardown delete failed")
					continue
				}
				deletedCount.Add(1)
			}
		}()
	}
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			failedCount.Add(1)
		}
	}
	close(jobs)
	wg.Wait()
	return deletedCount.Load(), failedCount.Load()
}
