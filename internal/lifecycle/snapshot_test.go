package lifecycle

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

func snapshotRunner(client *fakeClient) Runner {
	return Runner{
		Config: config.Config{
			Mode:                   config.ModeSnapshot,
			Target:                 "staging-us-central1",
			Environment:            "staging",
			Region:                 "us-central1",
			SandboxTemplate:        "superserve/python-3.11",
			ResourceTTL:            time.Hour,
			RunTimeout:             time.Second,
			LockTTL:                time.Minute,
			PollInterval:           time.Millisecond,
			CommandTimeout:         20 * time.Millisecond,
			DeleteTimeout:          20 * time.Millisecond,
			RetainFailedSandboxTTL: 2 * time.Hour,
		},
		Client:  client,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}
}

func TestRunSnapshotForksFromSnapshotVerifiesForkAndCleansUpInOrder(t *testing.T) {
	var order []string
	var memoryToken string
	polls := 0
	client := &fakeClient{
		createSandboxFn: func(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			if req.FromSnapshot == "" {
				order = append(order, "create_source")
				if req.AutoDeleteSeconds != 2*int(time.Hour.Seconds()) {
					t.Fatalf("source auto-delete must trail the janitor's expiry, got %d", req.AutoDeleteSeconds)
				}
				return canaryapi.Sandbox{ID: "sb-source", Status: "active", AccessToken: "src-tok"}, nil
			}
			order = append(order, "create_fork")
			if req.FromSnapshot != "snap-1" || req.FromTemplate != "" || req.AutoDeleteSeconds != int(time.Hour.Seconds()) {
				t.Fatalf("fork must be created from the snapshot only: %+v", req)
			}
			return canaryapi.Sandbox{ID: "sb-fork", Status: "active", AccessToken: "fork-tok", SourceSnapshotID: "snap-1"}, nil
		},
		getSandboxFn: func(_ context.Context, id string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: id, Status: "active"}, nil
		},
		writeFileFn: func(context.Context, string, string, string, []byte) error { return nil },
		execFn: func(_ context.Context, sandboxID, _ string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			switch {
			case strings.Contains(req.Command, "time.sleep(3600)"):
				memoryToken = req.Command[strings.Index(req.Command, "mem-"):strings.Index(req.Command, " >/tmp")]
				order = append(order, "seed:"+sandboxID)
			case req.Command == "true":
				order = append(order, "source_exec:"+sandboxID)
			case strings.Contains(req.Command, "verify_disk.sh"):
				order = append(order, "verify_disk:"+sandboxID)
			case strings.Contains(req.Command, "verify_memory.py"):
				if !strings.Contains(req.Command, memoryToken) {
					t.Fatalf("fork memory check must look for the source's token: %s", req.Command)
				}
				order = append(order, "verify_memory:"+sandboxID)
			default:
				t.Fatalf("unexpected exec command: %s", req.Command)
			}
			return canaryapi.ExecResult{ExitCode: 0}, nil
		},
		createSnapshotFn: func(_ context.Context, sandboxID string, req canaryapi.CreateSnapshotRequest) (canaryapi.Snapshot, error) {
			order = append(order, "snapshot:"+sandboxID)
			if req.Kind != "mem+fs" || req.IdempotencyKey == "" {
				t.Fatalf("unexpected snapshot request %+v", req)
			}
			return canaryapi.Snapshot{ID: "snap-1", SandboxID: sandboxID, Status: "creating"}, nil
		},
		getSnapshotFn: func(_ context.Context, id string) (canaryapi.Snapshot, error) {
			polls++
			status := "creating"
			if polls > 1 {
				status = "ready"
			}
			return canaryapi.Snapshot{ID: id, Status: status}, nil
		},
		updateSandboxFn: func(_ context.Context, id string, req canaryapi.UpdateSandboxRequest) error {
			if id != "sb-source" || req.Metadata[sandboxmetadata.KeySnapshotID] != "snap-1" || req.Metadata[sandboxmetadata.KeyManagedBy] == "" {
				t.Fatalf("snapshot id must be recorded on the source with full metadata: %s %+v", id, req.Metadata)
			}
			order = append(order, "record_snapshot_id")
			return nil
		},
		deleteSandboxFn: func(_ context.Context, id string) error {
			order = append(order, "delete:"+id)
			return nil
		},
		deleteSnapshotFn: func(_ context.Context, id string) error {
			order = append(order, "delete_snapshot:"+id)
			return nil
		},
	}

	if err := snapshotRunner(client).Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	want := []string{
		"create_source", "seed:sb-source", "snapshot:sb-source", "record_snapshot_id", "source_exec:sb-source",
		"create_fork", "verify_disk:sb-fork", "verify_memory:sb-fork",
		"delete:sb-fork", "delete_snapshot:snap-1", "delete:sb-source",
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order =\n%v\nwant\n%v", order, want)
	}
	if polls < 2 {
		t.Fatalf("expected the 202 path to poll until ready, polls = %d", polls)
	}
}

func TestRunSnapshotRecordsEveryDocumentedStep(t *testing.T) {
	client := &fakeClient{
		createSandboxFn: func(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-" + req.FromSnapshot, Status: "active", AccessToken: "tok", SourceSnapshotID: req.FromSnapshot}, nil
		},
		getSandboxFn: func(_ context.Context, id string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: id, Status: "active"}, nil
		},
		writeFileFn: func(context.Context, string, string, string, []byte) error { return nil },
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{ExitCode: 0}, nil
		},
		createSnapshotFn: func(_ context.Context, sandboxID string, _ canaryapi.CreateSnapshotRequest) (canaryapi.Snapshot, error) {
			return canaryapi.Snapshot{ID: "snap-1", SandboxID: sandboxID, Status: "ready"}, nil
		},
		getSnapshotFn: func(_ context.Context, id string) (canaryapi.Snapshot, error) {
			return canaryapi.Snapshot{ID: id, Status: "ready"}, nil
		},
	}
	recorder := &lifecycleMetricsRecorder{}
	r := snapshotRunner(client)
	r.Metrics = recorder

	if res := r.runSnapshot(context.Background(), "run-1"); res.Err != nil {
		t.Fatalf("runSnapshot returned %v", res.Err)
	}
	for _, step := range []string{
		"create_request", "create_wait_active", "create_total", "seed_canary_token", "initial_command",
		"snapshot_request", "snapshot_wait_ready", "snapshot_total", "record_snapshot_id", "source_wait_active", "source_exec",
		"fork_request", "fork_wait_active", "fork_total", "prepare_verification_utilities", "verify_disk", "verify_memory",
		"delete_request", "snapshot_delete",
	} {
		if _, ok := recorder.durations[step]; !ok {
			t.Errorf("step %q was never recorded", step)
		}
	}
}

func TestRunSnapshotFailedCaptureReportsStepAndSkipsFork(t *testing.T) {
	var deleted []string
	client := &fakeClient{
		createSandboxFn: func(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			if req.FromSnapshot != "" {
				t.Fatal("no fork after a failed snapshot")
			}
			return canaryapi.Sandbox{ID: "sb-source", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(_ context.Context, id string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: id, Status: "active"}, nil
		},
		writeFileFn: func(context.Context, string, string, string, []byte) error { return nil },
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{ExitCode: 0}, nil
		},
		createSnapshotFn: func(_ context.Context, sandboxID string, _ canaryapi.CreateSnapshotRequest) (canaryapi.Snapshot, error) {
			return canaryapi.Snapshot{ID: "snap-1", SandboxID: sandboxID, Status: "creating"}, nil
		},
		getSnapshotFn: func(_ context.Context, id string) (canaryapi.Snapshot, error) {
			return canaryapi.Snapshot{ID: id, Status: "failed"}, nil
		},
		deleteSandboxFn: func(_ context.Context, id string) error {
			deleted = append(deleted, id)
			return nil
		},
		deleteSnapshotFn: func(_ context.Context, id string) error {
			deleted = append(deleted, id)
			return nil
		},
	}

	res := snapshotRunner(client).runSnapshot(context.Background(), "run-1")
	if res.Err == nil || res.FailedStep != "snapshot_wait_ready" || res.SnapshotID != "snap-1" {
		t.Fatalf("failed step = %q snapshot = %q err = %v", res.FailedStep, res.SnapshotID, res.Err)
	}
	if strings.Join(deleted, ",") != "snap-1,sb-source" {
		t.Fatalf("cleanup = %v, want snapshot then source", deleted)
	}
}

func TestRunSnapshotRetainsSnapshotPointerAndSkipsSourceDeleteWhenSnapshotDeleteFails(t *testing.T) {
	var retained map[string]string
	retainedAutoDelete := 0
	var deletedSandboxes []string
	base := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-source", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(_ context.Context, id string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: id, Status: "active"}, nil
		},
		writeFileFn: func(context.Context, string, string, string, []byte) error { return nil },
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{ExitCode: 0}, nil
		},
		createSnapshotFn: func(_ context.Context, sandboxID string, _ canaryapi.CreateSnapshotRequest) (canaryapi.Snapshot, error) {
			return canaryapi.Snapshot{ID: "snap-1", SandboxID: sandboxID, Status: "creating"}, nil
		},
		getSnapshotFn: func(_ context.Context, id string) (canaryapi.Snapshot, error) {
			return canaryapi.Snapshot{ID: id, Status: "failed"}, nil
		},
		updateSandboxFn: func(_ context.Context, _ string, req canaryapi.UpdateSandboxRequest) error {
			retained = req.Metadata
			if req.AutoDeleteSeconds != nil {
				retainedAutoDelete = *req.AutoDeleteSeconds
			}
			return nil
		},
		deleteSandboxFn: func(_ context.Context, id string) error {
			deletedSandboxes = append(deletedSandboxes, id)
			return nil
		},
	}

	// Retention on: the capture failed after the id was recorded, and the
	// retention update must not erase the pointer.
	r := snapshotRunner(base)
	r.Config.RetainFailedSandbox = true
	res := r.runSnapshot(context.Background(), "run-1")
	if res.Err == nil || retained[sandboxmetadata.KeySnapshotID] != "snap-1" || retained[sandboxmetadata.KeyRetainedForDebug] != "true" {
		t.Fatalf("retention metadata must keep the snapshot pointer: %v (err=%v)", retained, res.Err)
	}
	if len(deletedSandboxes) != 0 {
		t.Fatalf("retained run must not delete the source: %v", deletedSandboxes)
	}
	if retainedAutoDelete != 2*int(r.Config.RetainFailedSandboxTTL.Seconds()) {
		t.Fatalf("retention must keep the auto-delete lead over the janitor, got %d", retainedAutoDelete)
	}

	// Retention off: a failed snapshot delete leaves the source for the janitor.
	base.deleteSnapshotFn = func(context.Context, string) error { return errors.New("409 snapshot is still being created") }
	res = snapshotRunner(base).runSnapshot(context.Background(), "run-2")
	if res.Err == nil || len(deletedSandboxes) != 0 {
		t.Fatalf("source must survive a failed snapshot delete: deleted=%v err=%v", deletedSandboxes, res.Err)
	}

	// Retention on but the pointer never persisted: nothing can find the
	// snapshot later, so it is deleted instead of retained.
	var deletedSnapshots []string
	base.deleteSnapshotFn = func(_ context.Context, id string) error {
		deletedSnapshots = append(deletedSnapshots, id)
		return nil
	}
	base.updateSandboxFn = func(context.Context, string, canaryapi.UpdateSandboxRequest) error {
		return errors.New("metadata service unavailable")
	}
	res = r.runSnapshot(context.Background(), "run-3")
	if res.FailedStep != "record_snapshot_id" || strings.Join(deletedSnapshots, ",") != "snap-1" {
		t.Fatalf("unrecorded snapshot must be deleted: step=%q deleted=%v", res.FailedStep, deletedSnapshots)
	}
}

func TestCreateSnapshotRetriesLostResponseWithSameIdempotencyKey(t *testing.T) {
	var keys []string
	client := &fakeClient{
		createSnapshotFn: func(_ context.Context, _ string, req canaryapi.CreateSnapshotRequest) (canaryapi.Snapshot, error) {
			keys = append(keys, req.IdempotencyKey)
			if len(keys) == 1 {
				return canaryapi.Snapshot{}, io.ErrUnexpectedEOF
			}
			return canaryapi.Snapshot{ID: "snap-1", Status: "ready"}, nil
		},
	}
	ops := Operations{Client: client, Metrics: metrics.NoopProvider{}}

	snap, err := ops.CreateSnapshot(context.Background(), "sb-1", canaryapi.CreateSnapshotRequest{Kind: "mem+fs", IdempotencyKey: "run-1"}, TelemetryContext{})
	if err != nil || snap.ID != "snap-1" {
		t.Fatalf("expected the retry to recover the snapshot, got %+v err=%v", snap, err)
	}
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("retry must reuse the idempotency key, got %v", keys)
	}
}
