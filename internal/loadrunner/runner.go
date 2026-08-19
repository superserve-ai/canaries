package loadrunner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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

type Summary struct {
	Started   int64
	Completed int64
	Failed    int64
	InFlight  int64
}

type operationResult struct {
	SandboxID              string
	StartedAt              time.Time
	CompletedAt            time.Time
	LifecycleDuration      time.Duration
	CreateRequestDuration  time.Duration
	WaitActiveDuration     time.Duration
	CreateToActiveDuration time.Duration
	ExecDuration           time.Duration
	DeleteDuration         time.Duration
	ReachedActive          bool
}

type lifecycleFailure struct {
	stage     string
	sandboxID string
	err       error
}

func (e *lifecycleFailure) Error() string { return e.err.Error() }
func (e *lifecycleFailure) Unwrap() error { return e.err }

func (r Runner) Run(ctx context.Context) (Summary, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Config.RunTimeout)
	defer cancel()

	start := r.now()
	jobs := make(chan int)
	var summary Summary
	var wg sync.WaitGroup
	for worker := 0; worker < r.Config.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for operation := range jobs {
				if ctx.Err() != nil {
					return
				}
				atomic.AddInt64(&summary.Started, 1)
				atomic.AddInt64(&summary.InFlight, 1)
				result, err := r.runOne(ctx, operation)
				inFlight := atomic.AddInt64(&summary.InFlight, -1)
				if err != nil {
					atomic.AddInt64(&summary.Failed, 1)
					r.logOperationResult(operation, result, err, inFlight)
					continue
				}
				atomic.AddInt64(&summary.Completed, 1)
				r.logOperationResult(operation, result, nil, inFlight)
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

	log.Info().Str("run_id", r.Config.RunID).Str("environment", r.Config.Environment).Str("region", r.Config.Region).Str("target", r.Config.Target).Int("configured_operations", r.Config.Operations).Int("configured_concurrency", r.Config.Concurrency).Int64("started", summary.Started).Int64("completed", summary.Completed).Int64("failed", summary.Failed).Int64("in_flight", summary.InFlight).Dur("duration", r.now().Sub(start)).Msg("load-test run complete")
	if ctx.Err() != nil {
		return summary, ctx.Err()
	}
	if summary.Failed > 0 {
		return summary, fmt.Errorf("%d of %d started lifecycles failed", summary.Failed, summary.Started)
	}
	return summary, nil
}

func (r Runner) runOne(ctx context.Context, operation int) (result operationResult, err error) {
	result.StartedAt = r.now().UTC()
	createdAt := result.StartedAt
	expiresAt := createdAt.Add(r.Config.ResourceTTL)
	defer func() {
		result.CompletedAt = r.now().UTC()
		result.LifecycleDuration = result.CompletedAt.Sub(result.StartedAt)
	}()

	telemetry := lifecycle.TelemetryContext{Environment: r.Config.Environment, Region: r.Config.Region, Target: r.Config.Target, Scenario: "loadtest-create"}
	metadata := sandboxmetadata.TestOwnershipMetadata(map[string]string{
		sandboxmetadata.KeyEnvironment: r.Config.Environment,
		sandboxmetadata.KeyRegion:      r.Config.Region,
	}, sandboxmetadata.TestOwnership{TestType: sandboxmetadata.TestTypeLoadTest, RunID: r.Config.RunID, WorkerID: r.Config.WorkerID, CreatedAt: createdAt, ExpiresAt: expiresAt})
	name := sandboxNameForOperation(r.Config.RunID, r.Config.WorkerID, operation)
	// The API omits zero-valued TTL fields, so preserve a positive configured
	// bound when ResourceTTL is shorter than one second.
	ttlSeconds := int((r.Config.ResourceTTL + time.Second - 1) / time.Second)
	createStarted := r.now()
	sb, createErr := r.Ops.CreateSandbox(ctx, lifecycle.CreateSandboxOptions{Request: canaryapi.CreateSandboxRequest{Name: name, FromTemplate: r.Config.Template, TimeoutSeconds: ttlSeconds, AutoDeleteSeconds: ttlSeconds, Metadata: metadata}, Telemetry: telemetry})
	result.CreateRequestDuration = r.now().Sub(createStarted)
	if createErr != nil {
		err = &lifecycleFailure{stage: "create_request", err: createErr}
		return
	}
	result.SandboxID = sb.ID

	cleanup := func() error {
		deleteStarted := r.now()
		defer func() {
			result.DeleteDuration = r.now().Sub(deleteStarted)
		}()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), r.Config.DeleteTimeout)
		defer cancel()
		cleanupErr := r.Ops.DeleteSandboxBestEffort(cleanupCtx, sb.ID, lifecycle.DeleteSandboxOptions{Timeout: r.Config.DeleteTimeout, Telemetry: telemetry})
		if cleanupErr != nil {
			log.Error().Err(cleanupErr).Int("operation", operation).Str("failed_stage", "delete_request").Str("sandbox_id", sb.ID).Str("run_id", r.Config.RunID).Str("worker_id", r.Config.WorkerID).Str("environment", r.Config.Environment).Msg("load-test cleanup failed; janitor will reclaim sandbox after expiry")
			return &lifecycleFailure{stage: "delete_request", sandboxID: sb.ID, err: cleanupErr}
		}
		return nil
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	waitStarted := r.now()
	if waitErr := r.Ops.WaitForStatusTimed(ctx, sb.ID, lifecycle.WaitForStatusOptions{Want: "active", Step: "wait_active", PollInterval: r.Config.PollInterval, Timeout: r.Config.ActiveTimeout, Telemetry: telemetry}); waitErr != nil {
		result.WaitActiveDuration = r.now().Sub(waitStarted)
		err = &lifecycleFailure{stage: "wait_active", sandboxID: sb.ID, err: waitErr}
		return
	}
	result.WaitActiveDuration = r.now().Sub(waitStarted)
	result.CreateToActiveDuration = r.now().Sub(result.StartedAt)
	result.ReachedActive = true

	execStarted := r.now()
	res, execErr := r.Ops.ExecStep(ctx, sb.ID, sb.AccessToken, lifecycle.ExecStepOptions{Step: "verify_exec", Command: "printf superserve-load-test", Timeout: r.Config.CommandTimeout, Telemetry: telemetry})
	result.ExecDuration = r.now().Sub(execStarted)
	if execErr != nil {
		err = &lifecycleFailure{stage: "verify_exec", sandboxID: sb.ID, err: execErr}
		return
	}
	if res.Stdout != "superserve-load-test" {
		err = &lifecycleFailure{stage: "verify_exec", sandboxID: sb.ID, err: fmt.Errorf("unexpected exec output %q", res.Stdout)}
		return
	}
	return
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
		event = event.Str("sandbox_id", result.SandboxID).Dur("delete_duration", result.DeleteDuration)
	}
	if result.WaitActiveDuration > 0 {
		event = event.Dur("wait_active_duration", result.WaitActiveDuration)
	}
	if result.ReachedActive {
		event = event.Dur("create_to_active_duration", result.CreateToActiveDuration).Dur("exec_duration", result.ExecDuration)
	}
	if err != nil {
		failure := &lifecycleFailure{}
		if errors.As(err, &failure) {
			event = event.Str("failed_stage", failure.stage)
			if result.SandboxID == "" && failure.sandboxID != "" {
				event = event.Str("sandbox_id", failure.sandboxID)
			}
		}
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
