package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

// A sandbox that names a snapshot must outlive the janitor sweep that clears
// the pointer, so platform auto-delete trails the janitor's expiry by this much.
const snapshotPointerAutoDeleteFactor = 2

type WaitForSnapshotOptions struct {
	PollInterval time.Duration
	Telemetry    TelemetryContext
}

// runSnapshot seeds a source sandbox, snapshots it, forks the snapshot, and
// runs the lifecycle disk and memory checks inside the fork. The memory check
// is what proves the fork restored process state and not just the filesystem.
func (r Runner) runSnapshot(ctx context.Context, runID string) (res RunResult) {
	ops := r.operations()
	telemetry := r.telemetry()
	source := RunResources{RunID: runID, CreatedAt: r.Clock().UTC()}
	fork := RunResources{RunID: runID, CreatedAt: source.CreatedAt}
	pointerRecorded := false
	defer func() {
		if fork.SandboxID != "" {
			res.Err = r.FinalizeSandbox(context.Background(), fork, res)
		}
		// The source is the only pointer to the snapshot, so the snapshot goes
		// first and a failed snapshot delete leaves the source for the janitor.
		// Retention keeps the snapshot only once the source actually names it.
		keepSnapshot := res.Err != nil && r.Config.RetainFailedSandbox && pointerRecorded
		if res.SnapshotID != "" && !keepSnapshot {
			logStep("snapshot_delete")
			if err := ops.DeleteSnapshotBestEffort(context.Background(), res.SnapshotID, DeleteSandboxOptions{Timeout: r.Config.DeleteTimeout, Telemetry: telemetry}); err != nil {
				log.Warn().Err(err).Str("snapshot_id", res.SnapshotID).Str("sandbox_id", source.SandboxID).Msg("snapshot delete failed; source left for the janitor")
				if res.Err == nil {
					res.Err = err
				}
				return
			}
		}
		if source.SandboxID != "" {
			res.Err = r.FinalizeSandbox(context.Background(), source, res)
		}
	}()

	createStart := r.Clock()
	logStep("create_request")
	req := r.canaryCreateSandboxRequest(source)
	req.AutoDeleteSeconds *= snapshotPointerAutoDeleteFactor
	sb, err := ops.CreateSandbox(ctx, CreateSandboxOptions{Request: req, Telemetry: telemetry})
	if err != nil {
		ops.RecordStep(ctx, telemetry, "create_total", result(err), r.Clock().Sub(createStart))
		return failStep(res, StepError{Step: "create_request", Err: err})
	}
	source.SandboxID = sb.ID
	res.SandboxID = sb.ID

	logStep("create_wait_active")
	if err := ops.WaitForStatusTimed(ctx, sb.ID, r.waitOptions("active", "create_wait_active", telemetry)); err != nil {
		ops.RecordStep(ctx, telemetry, "create_total", result(err), r.Clock().Sub(createStart))
		return failStep(res, StepError{Step: "create_wait_active", Err: fmt.Errorf("waiting for sandbox to become active: %w", err)})
	}
	ops.RecordStep(ctx, telemetry, "create_total", "success", r.Clock().Sub(createStart))

	state, err := r.seedState(ctx, sb, telemetry)
	if err != nil {
		return failStep(res, err)
	}

	snapshotStart := r.Clock()
	logStep("snapshot_request")
	snap, err := ops.CreateSnapshot(ctx, sb.ID, canaryapi.CreateSnapshotRequest{
		Kind:           "mem+fs",
		Name:           "api-canary-" + runID,
		IdempotencyKey: runID,
	}, telemetry)
	requestDuration := r.Clock().Sub(snapshotStart)
	if err != nil {
		ops.RecordStep(ctx, telemetry, "snapshot_total", result(err), requestDuration)
		return failStep(res, StepError{Step: "snapshot_request", Err: err})
	}
	res.SnapshotID = snap.ID
	source.RetainMetadata = map[string]string{sandboxmetadata.KeySnapshotID: snap.ID}

	// Point the source at its snapshot before anything else can fail, so a
	// retained or orphaned source still leads the janitor to it.
	logStep("record_snapshot_id")
	req.Metadata[sandboxmetadata.KeySnapshotID] = snap.ID
	recordStart := r.Clock()
	err = r.Client.UpdateSandbox(ctx, sb.ID, canaryapi.UpdateSandboxRequest{Metadata: req.Metadata})
	ops.RecordStep(ctx, telemetry, "record_snapshot_id", result(err), r.Clock().Sub(recordStart))
	if err != nil {
		return failStep(res, StepError{Step: "record_snapshot_id", Err: fmt.Errorf("recording snapshot id: %w", err)})
	}
	pointerRecorded = true

	waitStart := r.Clock()
	logStep("snapshot_wait_ready")
	if _, err := ops.WaitForSnapshotReady(ctx, snap.ID, WaitForSnapshotOptions{PollInterval: r.Config.PollInterval, Telemetry: telemetry}); err != nil {
		ops.RecordStep(ctx, telemetry, "snapshot_total", result(err), requestDuration+r.Clock().Sub(waitStart))
		return failStep(res, StepError{Step: "snapshot_wait_ready", Err: fmt.Errorf("waiting for snapshot: %w", err)})
	}
	ops.RecordStep(ctx, telemetry, "snapshot_total", "success", requestDuration+r.Clock().Sub(waitStart))

	// Capturing an active sandbox pauses it briefly; the source must come back.
	logStep("source_wait_active")
	if err := ops.WaitForStatusTimed(ctx, sb.ID, r.waitOptions("active", "source_wait_active", telemetry)); err != nil {
		return failStep(res, StepError{Step: "source_wait_active", Err: fmt.Errorf("waiting for source after snapshot: %w", err)})
	}
	logStep("source_exec")
	if _, err := ops.ExecStep(ctx, sb.ID, sb.AccessToken, ExecStepOptions{
		Step:      "source_exec",
		Command:   "true",
		Timeout:   r.Config.CommandTimeout,
		Telemetry: telemetry,
	}); err != nil {
		return failStep(res, StepError{Step: "source_exec", Err: fmt.Errorf("source exec after snapshot: %w", err)})
	}

	forkStart := r.Clock()
	logStep("fork_request")
	forkReq := canaryapi.CreateSandboxRequest{
		Name:              sandboxName(r.Config.Target+"-fork", runID),
		FromSnapshot:      snap.ID,
		TimeoutSeconds:    req.TimeoutSeconds,
		AutoDeleteSeconds: int(r.retentionTTL().Seconds()),
		Metadata:          sandboxmetadata.LegacyCanaryMetadata(r.Config.Environment, r.Config.Region, r.Config.Target, runID, fork.CreatedAt, fork.CreatedAt.Add(r.retentionTTL()).UTC()),
	}
	forked, err := ops.CreateSandbox(ctx, CreateSandboxOptions{Request: forkReq, Step: "fork_request", Telemetry: telemetry})
	if err == nil && forked.SourceSnapshotID != snap.ID {
		err = fmt.Errorf("fork reports source_snapshot_id %q, want %q", forked.SourceSnapshotID, snap.ID)
	}
	if err != nil {
		ops.RecordStep(ctx, telemetry, "fork_total", result(err), r.Clock().Sub(forkStart))
		if forked.ID != "" {
			fork.SandboxID = forked.ID
			res.ForkID = forked.ID
		}
		return failStep(res, StepError{Step: "fork_request", Err: err})
	}
	fork.SandboxID = forked.ID
	res.ForkID = forked.ID

	logStep("fork_wait_active")
	if err := ops.WaitForStatusTimed(ctx, forked.ID, r.waitOptions("active", "fork_wait_active", telemetry)); err != nil {
		ops.RecordStep(ctx, telemetry, "fork_total", result(err), r.Clock().Sub(forkStart))
		return failStep(res, StepError{Step: "fork_wait_active", Err: fmt.Errorf("waiting for fork to become active: %w", err)})
	}
	ops.RecordStep(ctx, telemetry, "fork_total", "success", r.Clock().Sub(forkStart))

	if err := r.verifyState(ctx, forked.ID, forked.AccessToken, state, telemetry); err != nil {
		return failStep(res, err)
	}
	return res
}

func (r Runner) waitOptions(want, step string, telemetry TelemetryContext) WaitForStatusOptions {
	return WaitForStatusOptions{
		Want:         want,
		Step:         step,
		PollInterval: r.Config.PollInterval,
		Timeout:      r.Config.CommandTimeout,
		Telemetry:    telemetry,
	}
}

func (o Operations) CreateSnapshot(ctx context.Context, sandboxID string, req canaryapi.CreateSnapshotRequest, telemetry TelemetryContext) (canaryapi.Snapshot, error) {
	start := o.now()
	var snap canaryapi.Snapshot
	// A lost response may hide a committed capture; the idempotency key makes
	// the retry return that snapshot instead of leaking it.
	err := retryTransient(ctx, func() error {
		var attemptErr error
		snap, attemptErr = o.Client.CreateSnapshot(ctx, sandboxID, req)
		return attemptErr
	})
	if err == nil && snap.ID == "" {
		err = errors.New("snapshot response has no id")
	}
	o.recordStep(ctx, telemetry, "snapshot_request", result(err), o.now().Sub(start))
	if err != nil {
		return canaryapi.Snapshot{}, fmt.Errorf("creating snapshot: %w", err)
	}
	return snap, nil
}

// WaitForSnapshotReady covers the 202 path, where the host's answer was lost
// and the row settles later; a 201 already reads ready on the first poll.
func (o Operations) WaitForSnapshotReady(ctx context.Context, snapshotID string, opts WaitForSnapshotOptions) (canaryapi.Snapshot, error) {
	start := o.now()
	snap, err := o.waitForSnapshotReady(ctx, snapshotID, opts)
	o.recordStep(ctx, opts.Telemetry, "snapshot_wait_ready", result(err), o.now().Sub(start))
	return snap, err
}

func (o Operations) waitForSnapshotReady(ctx context.Context, snapshotID string, opts WaitForSnapshotOptions) (canaryapi.Snapshot, error) {
	interval := opts.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		snap, err := o.Client.GetSnapshot(ctx, snapshotID)
		if err != nil {
			return snap, fmt.Errorf("fetching snapshot status: %w", err)
		}
		switch snap.Status {
		case "ready":
			return snap, nil
		case "failed", "deleting":
			return snap, fmt.Errorf("snapshot entered terminal state %q", snap.Status)
		}
		select {
		case <-ctx.Done():
			return snap, fmt.Errorf("waiting for snapshot (last=%s): %w", snap.Status, ctx.Err())
		case <-time.After(interval):
		}
	}
}

func (o Operations) DeleteSnapshotBestEffort(ctx context.Context, snapshotID string, opts DeleteSandboxOptions) error {
	start := o.now()
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	err := o.Client.DeleteSnapshot(ctx, snapshotID)
	if errors.Is(err, canaryapi.ErrNotFound) {
		err = nil
	}
	o.RecordCleanup(ctx, opts.Telemetry, result(err))
	o.recordStep(ctx, opts.Telemetry, "snapshot_delete", result(err), o.now().Sub(start))
	if err != nil {
		return fmt.Errorf("deleting snapshot: %w", err)
	}
	return nil
}
