package loadrunner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

type Runner struct {
	Config Config
	Ops    lifecycle.Operations
	Clock  func() time.Time
}

// PhaseSummary retains every successful completion latency, including phases
// belonging to a lifecycle that subsequently fails. Config caps runs at 10,000.
type PhaseSummary struct {
	Attempts  int64
	Failures  int64
	Durations []time.Duration
}

type Summary struct {
	Started               int64
	Completed             int64
	Failed                int64
	InFlight              int64
	ConfiguredOperations  int64
	ConfiguredConcurrency int64
	PeakInFlight          int64
	Unstarted             int64
	Duration              time.Duration
	Create                PhaseSummary
	Pause                 PhaseSummary
	Resume                PhaseSummary
	Delete                PhaseSummary
	LifecycleDurations    []time.Duration
	FailurePhases         map[string]int64
	CleanupFailures       int64
}

type phaseResult struct {
	Attempted bool
	Succeeded bool
	Duration  time.Duration
}

type operationResult struct {
	SandboxID                string
	StartedAt                time.Time
	CompletedAt              time.Time
	LifecycleDuration        time.Duration
	CreateRequestDuration    time.Duration
	WaitActiveDuration       time.Duration
	CreateToActiveDuration   time.Duration
	ExecDuration             time.Duration
	PauseRequestDuration     time.Duration
	WaitPausedDuration       time.Duration
	ResumeRequestDuration    time.Duration
	ResumeWaitDuration       time.Duration
	ResumeExecDuration       time.Duration
	DeleteRequestDuration    time.Duration
	DeleteDuration           time.Duration
	ReachedActive            bool
	Create                   phaseResult
	Pause                    phaseResult
	Resume                   phaseResult
	Delete                   phaseResult
	FailureStages            []string
	CleanupFailed            bool
	DeleteCompletionObserved bool
	UncertainCreate          bool
}

type lifecycleFailure struct {
	stage     string
	sandboxID string
	err       error
}

func (e *lifecycleFailure) Error() string { return e.err.Error() }
func (e *lifecycleFailure) Unwrap() error { return e.err }

func (r Runner) Run(parent context.Context) (Summary, error) {
	if r.Config.Operations <= 0 || r.Config.Operations > MaxOperations || r.Config.Concurrency <= 0 || r.Config.Concurrency > r.Config.Operations {
		return Summary{ConfiguredOperations: int64(r.Config.Operations), ConfiguredConcurrency: int64(r.Config.Concurrency), Unstarted: int64(r.Config.Operations)}, fmt.Errorf("load runner requires 1..%d operations and concurrency between 1 and the operation count", MaxOperations)
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(parent, r.Config.RunTimeout)
	defer timeoutCancel()
	ctx, cancel := context.WithCancel(timeoutCtx)
	defer cancel()

	start := r.now()
	jobs := make(chan int)
	summary := Summary{ConfiguredOperations: int64(r.Config.Operations), ConfiguredConcurrency: int64(r.Config.Concurrency), FailurePhases: make(map[string]int64)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	replacementUnsafe := false
	stopDispatch := func() {
		mu.Lock()
		replacementUnsafe = true
		// Set the stop flag under the same lock as worker admission. Once a
		// cleanup failure or unknown create outcome is known, no replacement
		// sandbox may be created.
		mu.Unlock()
		cancel()
	}
	for worker := 0; worker < r.Config.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for operation := range jobs {
				mu.Lock()
				if ctx.Err() != nil || replacementUnsafe {
					mu.Unlock()
					return
				}
				summary.Started++
				summary.InFlight++
				if summary.InFlight > summary.PeakInFlight {
					summary.PeakInFlight = summary.InFlight
				}
				mu.Unlock()
				result, err := r.runOneWithUnsafeReplacement(ctx, operation, stopDispatch)
				mu.Lock()
				summary.InFlight--
				inFlight := summary.InFlight
				summary.addResult(result, err)
				mu.Unlock()
				r.logOperationResult(operation, result, err, inFlight)
			}
		}()
	}

sendLoop:
	for i := 0; i < r.Config.Operations; i++ {
		select {
		case <-ctx.Done():
			break sendLoop
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	summary.Duration = r.now().Sub(start)
	summary.Unstarted = summary.ConfiguredOperations - summary.Started

	log.Info().Str("run_id", r.Config.RunID).Str("environment", r.Config.Environment).Str("region", r.Config.Region).Str("target", r.Config.Target).Int("configured_operations", r.Config.Operations).Int("configured_concurrency", r.Config.Concurrency).Int64("peak_in_flight", summary.PeakInFlight).Int64("started", summary.Started).Int64("completed", summary.Completed).Int64("failed", summary.Failed).Int64("unstarted", summary.Unstarted).Int64("in_flight", summary.InFlight).Int64("cleanup_failures", summary.CleanupFailures).Dur("duration", summary.Duration).Msg("load-test run complete")
	var runErr error
	if summary.Failed > 0 {
		runErr = fmt.Errorf("%d of %d started lifecycles failed; %d unstarted", summary.Failed, summary.Started, summary.Unstarted)
	}
	if timeoutCtx.Err() != nil {
		runErr = errors.Join(runErr, timeoutCtx.Err())
	}
	return summary, runErr
}

func (s *Summary) addResult(result operationResult, err error) {
	for _, phase := range []struct {
		summary *PhaseSummary
		result  phaseResult
		sample  bool
	}{{&s.Create, result.Create, true}, {&s.Pause, result.Pause, true}, {&s.Resume, result.Resume, true}, {&s.Delete, result.Delete, result.DeleteCompletionObserved}} {
		if !phase.result.Attempted {
			continue
		}
		phase.summary.Attempts++
		if !phase.result.Succeeded {
			phase.summary.Failures++
		} else if phase.sample {
			phase.summary.Durations = append(phase.summary.Durations, phase.result.Duration)
		}
	}
	for _, stage := range result.FailureStages {
		s.FailurePhases[stage]++
	}
	if result.CleanupFailed {
		s.CleanupFailures++
	}
	if err != nil {
		s.Failed++
	} else {
		s.Completed++
		s.LifecycleDurations = append(s.LifecycleDurations, result.LifecycleDuration)
	}
}

func (r Runner) runOne(ctx context.Context, operation int) (operationResult, error) {
	return r.runOneWithUnsafeReplacement(ctx, operation, nil)
}

func (r Runner) runOneWithUnsafeReplacement(ctx context.Context, operation int, onUnsafeReplacement func()) (result operationResult, err error) {
	result.StartedAt = r.now().UTC()
	createdAt := result.StartedAt
	expiresAt := createdAt.Add(r.Config.ResourceTTL)
	defer func() {
		result.CompletedAt = r.now().UTC()
		result.LifecycleDuration = result.CompletedAt.Sub(result.StartedAt)
	}()
	fail := func(stage string, cause error) error {
		result.FailureStages = append(result.FailureStages, stage)
		return &lifecycleFailure{stage: stage, sandboxID: result.SandboxID, err: cause}
	}

	fullLifecycle := r.Config.Scenario == "lifecycle"
	scenario := "loadtest-create"
	if fullLifecycle {
		scenario = "loadtest-lifecycle"
	}
	telemetry := lifecycle.TelemetryContext{Environment: r.Config.Environment, Region: r.Config.Region, Target: r.Config.Target, Scenario: scenario}
	metadata := sandboxmetadata.TestOwnershipMetadata(map[string]string{
		sandboxmetadata.KeyEnvironment: r.Config.Environment,
		sandboxmetadata.KeyRegion:      r.Config.Region,
	}, sandboxmetadata.TestOwnership{TestType: sandboxmetadata.TestTypeLoadTest, RunID: r.Config.RunID, WorkerID: r.Config.WorkerID, CreatedAt: createdAt, ExpiresAt: expiresAt})
	name := sandboxNameForOperation(r.Config.RunID, r.Config.WorkerID, operation)
	// The API omits zero-valued TTL fields, so preserve a positive configured
	// bound when ResourceTTL is shorter than one second.
	ttlSeconds := int((r.Config.ResourceTTL + time.Second - 1) / time.Second)
	createStarted := r.now()
	result.Create.Attempted = true
	request := canaryapi.CreateSandboxRequest{Name: name, FromTemplate: r.Config.Template, TimeoutSeconds: ttlSeconds, AutoDeleteSeconds: ttlSeconds, Metadata: metadata}
	if r.Config.Scheduled {
		// The API restarts timeout_seconds on resume. A fixed one-minute
		// active-session limit therefore bounds each session after a crash;
		// the longer resource expiry remains available to the janitor.
		request.TimeoutSeconds = 60
		idleSeconds := 60
		request.IdleTimeoutSeconds = &idleSeconds
	}
	sb, createErr := r.Ops.CreateSandbox(ctx, lifecycle.CreateSandboxOptions{Request: request, Telemetry: telemetry})
	result.CreateRequestDuration = r.now().Sub(createStarted)
	if createErr != nil {
		result.UncertainCreate = uncertainCreateOutcome(createErr)
		if result.UncertainCreate && onUnsafeReplacement != nil {
			onUnsafeReplacement()
		}
		err = fail("create_request", createErr)
		return
	}
	result.SandboxID = sb.ID
	if sb.ID == "" {
		result.UncertainCreate = true
		if onUnsafeReplacement != nil {
			onUnsafeReplacement()
		}
		err = fail("create_request", errors.New("create response did not contain a sandbox ID"))
		return
	}
	defer func() {
		result.Delete.Attempted = true
		deleteStarted := r.now()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), r.Config.DeleteTimeout)
		defer cancel()
		requestStarted := r.now()
		cleanupErr := r.Ops.DeleteSandboxBestEffort(cleanupCtx, sb.ID, lifecycle.DeleteSandboxOptions{Timeout: r.Config.DeleteTimeout, Telemetry: telemetry})
		result.DeleteRequestDuration = r.now().Sub(requestStarted)
		stage := "delete_request"
		if cleanupErr == nil && fullLifecycle {
			stage = "delete_wait_deleted"
			waitStarted := r.now()
			cleanupErr = r.waitDeleted(cleanupCtx, sb.ID)
			r.Ops.RecordStep(cleanupCtx, telemetry, stage, stepOutcome(cleanupErr), r.now().Sub(waitStarted))
		}
		result.DeleteDuration = r.now().Sub(deleteStarted)
		result.Delete.Duration = result.DeleteDuration
		result.Delete.Succeeded = cleanupErr == nil
		result.DeleteCompletionObserved = fullLifecycle && cleanupErr == nil
		if cleanupErr != nil {
			result.CleanupFailed = true
			if onUnsafeReplacement != nil {
				onUnsafeReplacement()
			}
			log.Error().Err(cleanupErr).Int("operation", operation).Str("failed_stage", stage).Str("sandbox_id", sb.ID).Str("run_id", r.Config.RunID).Str("worker_id", r.Config.WorkerID).Str("environment", r.Config.Environment).Msg("load-test cleanup failed; janitor will reclaim sandbox after expiry")
			err = errors.Join(err, fail(stage, cleanupErr))
		}
	}()

	waitStarted := r.now()
	waitErr := r.waitStatus(ctx, sb.ID, "active", "wait_active", telemetry)
	result.WaitActiveDuration = r.now().Sub(waitStarted)
	if waitErr != nil {
		err = fail("wait_active", waitErr)
		return
	}
	result.CreateToActiveDuration = r.now().Sub(createStarted)
	result.ReachedActive = true
	result.Create.Succeeded = true
	result.Create.Duration = result.CreateToActiveDuration

	execStarted := r.now()
	execErr := r.verifyExec(ctx, sb.ID, sb.AccessToken, "verify_exec", telemetry)
	result.ExecDuration = r.now().Sub(execStarted)
	if execErr != nil {
		err = fail("verify_exec", execErr)
		return
	}
	if !fullLifecycle {
		return
	}

	pauseStarted := r.now()
	result.Pause.Attempted = true
	pauseErr := r.Ops.Pause(ctx, sb.ID, telemetry)
	result.PauseRequestDuration = r.now().Sub(pauseStarted)
	result.Pause.Duration = result.PauseRequestDuration
	if pauseErr != nil {
		err = fail("pause_request", pauseErr)
		return
	}
	waitStarted = r.now()
	waitErr = r.waitStatus(ctx, sb.ID, "paused", "pause_wait_paused", telemetry)
	result.WaitPausedDuration = r.now().Sub(waitStarted)
	result.Pause.Duration = r.now().Sub(pauseStarted)
	if waitErr != nil {
		err = fail("pause_wait_paused", waitErr)
		return
	}
	result.Pause.Succeeded = true

	resumeStarted := r.now()
	result.Resume.Attempted = true
	resumed, resumeErr := r.Ops.Resume(ctx, sb.ID, telemetry)
	result.ResumeRequestDuration = r.now().Sub(resumeStarted)
	result.Resume.Duration = result.ResumeRequestDuration
	if resumeErr != nil {
		err = fail("resume_request", resumeErr)
		return
	}
	waitStarted = r.now()
	waitErr = r.waitStatus(ctx, sb.ID, "active", "resume_wait_active", telemetry)
	result.ResumeWaitDuration = r.now().Sub(waitStarted)
	result.Resume.Duration = r.now().Sub(resumeStarted)
	if waitErr != nil {
		err = fail("resume_wait_active", waitErr)
		return
	}
	result.Resume.Succeeded = true
	execStarted = r.now()
	execErr = r.verifyExec(ctx, sb.ID, resumed.AccessToken, "verify_exec_after_resume", telemetry)
	result.ResumeExecDuration = r.now().Sub(execStarted)
	if execErr != nil {
		err = fail("verify_exec_after_resume", execErr)
	}
	return
}

// A lost or undecodable response cannot prove that POST did not create a
// sandbox. Only a definite client rejection permits safe replacement work.
func uncertainCreateOutcome(err error) bool {
	if errors.Is(err, canaryapi.ErrNotFound) {
		return false
	}
	var statusErr *canaryapi.HTTPStatusError
	return !errors.As(err, &statusErr) || statusErr.StatusCode < http.StatusBadRequest || statusErr.StatusCode >= http.StatusInternalServerError || statusErr.StatusCode == http.StatusRequestTimeout
}

func (r Runner) waitStatus(ctx context.Context, sandboxID, want, step string, telemetry lifecycle.TelemetryContext) error {
	// Operations supports injected clocks; a real context deadline also bounds
	// waits when a test clock remains fixed.
	waitCtx, cancel := context.WithTimeout(ctx, r.Config.ActiveTimeout)
	defer cancel()
	return r.Ops.WaitForStatusTimed(waitCtx, sandboxID, lifecycle.WaitForStatusOptions{Want: want, Step: step, PollInterval: r.Config.PollInterval, Timeout: r.Config.ActiveTimeout, Telemetry: telemetry})
}

func (r Runner) waitDeleted(ctx context.Context, sandboxID string) error {
	pollInterval := r.Config.PollInterval
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for sandbox deletion: %w", err)
		}
		sb, err := r.Ops.Client.GetSandbox(ctx, sandboxID)
		if errors.Is(err, canaryapi.ErrNotFound) || (err == nil && sb.Status == "deleted") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("fetching sandbox deletion status: %w", err)
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for sandbox deletion: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (r Runner) verifyExec(ctx context.Context, sandboxID, accessToken, step string, telemetry lifecycle.TelemetryContext) (err error) {
	start := r.now()
	defer func() { r.Ops.RecordStep(ctx, telemetry, step, stepOutcome(err), r.now().Sub(start)) }()
	res, err := r.Ops.Exec(ctx, sandboxID, accessToken, canaryapi.ExecRequest{Command: "printf superserve-load-test", TimeoutS: int(r.Config.CommandTimeout.Seconds())})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exec command failed: exit=%d", res.ExitCode)
	}
	if res.Stdout != "superserve-load-test" {
		return fmt.Errorf("unexpected exec output %q", res.Stdout)
	}
	return nil
}

func stepOutcome(err error) string {
	if err != nil {
		return "failure"
	}
	return "success"
}

func (r Runner) logOperationResult(operation int, result operationResult, err error, inFlight int64) {
	event := log.Info()
	outcome := "success"
	if err != nil {
		event = log.Error().Err(err)
		outcome = "failure"
	}
	event = event.
		Int("operation", operation).
		Str("run_id", r.Config.RunID).
		Str("worker_id", r.Config.WorkerID).
		Str("environment", r.Config.Environment).
		Str("region", r.Config.Region).
		Str("target", r.Config.Target).
		Str("outcome", outcome).
		Int64("in_flight", inFlight).
		Time("started_at", result.StartedAt).
		Time("completed_at", result.CompletedAt).
		Dur("lifecycle_duration", result.LifecycleDuration).
		Dur("create_request_duration", result.CreateRequestDuration)
	if result.SandboxID != "" {
		event = event.Str("sandbox_id", result.SandboxID).Dur("delete_request_duration", result.DeleteRequestDuration).Dur("delete_duration", result.DeleteDuration)
	}
	if result.Create.Attempted {
		event = event.Bool("create_outcome_unknown", result.UncertainCreate).Dur("wait_active_duration", result.WaitActiveDuration)
	}
	if result.ReachedActive {
		event = event.Dur("create_to_active_duration", result.CreateToActiveDuration).Dur("exec_duration", result.ExecDuration)
	}
	if result.Pause.Attempted {
		event = event.Dur("pause_request_duration", result.PauseRequestDuration).Dur("wait_paused_duration", result.WaitPausedDuration).Dur("pause_duration", result.Pause.Duration)
	}
	if result.Resume.Attempted {
		event = event.Dur("resume_request_duration", result.ResumeRequestDuration).Dur("resume_wait_active_duration", result.ResumeWaitDuration).Dur("resume_duration", result.Resume.Duration).Dur("resume_exec_duration", result.ResumeExecDuration)
	}
	if len(result.FailureStages) > 0 {
		event = event.Str("failed_stage", result.FailureStages[0]).Strs("failed_stages", result.FailureStages)
	}
	event.Msg("load-test lifecycle complete")
}

func (r Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func sandboxNameForOperation(runID, workerID string, operation int) string {
	return fmt.Sprintf("loadtest-%s-%s-%06d", safeNamePart(runID, 20), safeNamePart(workerID, 12), operation)
}
