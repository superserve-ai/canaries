package loadrunner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/metrics"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

type fakeLoadClient struct {
	mu          sync.Mutex
	created     []canaryapi.CreateSandboxRequest
	deleted     []string
	active      int
	maxActive   int
	deleteErr   error
	createDelay time.Duration
	status      string
	execStdout  string
	statusCalls map[string]int
	waited      map[string]bool
}

func (f *fakeLoadClient) CreateSandbox(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
	if f.createDelay > 0 {
		time.Sleep(f.createDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	if f.statusCalls == nil {
		f.statusCalls = make(map[string]int)
	}
	if f.waited == nil {
		f.waited = make(map[string]bool)
	}
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	return canaryapi.Sandbox{ID: req.Name, Status: "creating", AccessToken: "token"}, nil
}
func (f *fakeLoadClient) GetSandbox(_ context.Context, id string) (canaryapi.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusCalls == nil {
		f.statusCalls = make(map[string]int)
	}
	f.statusCalls[id]++
	status := f.status
	if status == "" {
		if f.statusCalls[id] == 1 {
			status = "creating"
		} else {
			status = "active"
			f.waited[id] = true
		}
	}
	return canaryapi.Sandbox{ID: id, Status: status}, nil
}
func (f *fakeLoadClient) DeleteSandbox(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	f.active--
	return f.deleteErr
}
func (f *fakeLoadClient) Exec(_ context.Context, id string, _ string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.waited[id] {
		return canaryapi.ExecResult{}, errors.New("exec called before sandbox became active")
	}
	if req.Command != "printf superserve-load-test" {
		return canaryapi.ExecResult{}, errors.New("unexpected command")
	}
	stdout := f.execStdout
	if stdout == "" {
		stdout = "superserve-load-test"
	}
	return canaryapi.ExecResult{Stdout: stdout, ExitCode: 0}, nil
}
func (*fakeLoadClient) PauseSandbox(context.Context, string) error { return nil }
func (*fakeLoadClient) ResumeSandbox(context.Context, string) (canaryapi.ResumeResponse, error) {
	return canaryapi.ResumeResponse{}, nil
}
func (*fakeLoadClient) UpdateSandbox(context.Context, string, canaryapi.UpdateSandboxRequest) error {
	return nil
}
func (*fakeLoadClient) PublishPreviewPort(context.Context, string, canaryapi.PublishPreviewPortRequest) error {
	return nil
}
func (*fakeLoadClient) WriteFile(context.Context, string, string, string, []byte) error { return nil }
func (*fakeLoadClient) PreviewURL(string, int) string                                   { return "" }

func testRunnerConfig() Config {
	return Config{Environment: "staging", Region: "us-central1", Target: "staging-us-central1", Template: "template", RunID: "run-1", WorkerID: "worker-1", Operations: 8, Concurrency: 3, RunTimeout: time.Second, ResourceTTL: time.Hour, PollInterval: time.Millisecond, ActiveTimeout: time.Second, CommandTimeout: time.Second, DeleteTimeout: time.Second}
}

func TestRunnerRunsConcurrentDeterministicLifecycles(t *testing.T) {
	client := &fakeLoadClient{createDelay: time.Millisecond}
	cfg := testRunnerConfig()
	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: metrics.NoopProvider{}}, Clock: func() time.Time { return time.Unix(100, 0) }}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if summary.Started != int64(cfg.Operations) || summary.Completed != int64(cfg.Operations) || summary.Failed != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.maxActive > cfg.Concurrency {
		t.Fatalf("max concurrency %d exceeded configured %d", client.maxActive, cfg.Concurrency)
	}
	if len(client.deleted) != cfg.Operations {
		t.Fatalf("deleted %d sandboxes, want %d", len(client.deleted), cfg.Operations)
	}
	for _, req := range client.created {
		if req.Metadata[sandboxmetadata.KeyManagedBy] != sandboxmetadata.ManagedByTestRunner || req.Metadata[sandboxmetadata.KeyTestType] != sandboxmetadata.TestTypeLoadTest || req.Metadata[sandboxmetadata.KeyEnvironment] != cfg.Environment || req.Metadata[sandboxmetadata.KeyRegion] != cfg.Region || req.Metadata[sandboxmetadata.KeyRunID] != cfg.RunID || req.Metadata[sandboxmetadata.KeyWorkerID] != cfg.WorkerID {
			t.Fatalf("missing ownership metadata: %#v", req.Metadata)
		}
	}
	for _, id := range client.deleted {
		if client.statusCalls[id] < 2 || !client.waited[id] {
			t.Fatalf("sandbox %q was not observed active before exec: status calls=%d waited=%v", id, client.statusCalls[id], client.waited[id])
		}
	}
}

func TestRunnerRoundsSubsecondResourceTTLUpForCreateRequest(t *testing.T) {
	client := &fakeLoadClient{}
	cfg := testRunnerConfig()
	cfg.Operations = 1
	cfg.Concurrency = 1
	cfg.ResourceTTL = 500 * time.Millisecond

	if _, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: metrics.NoopProvider{}}, Clock: time.Now}).Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if got := client.created[0].TimeoutSeconds; got != 1 {
		t.Fatalf("TimeoutSeconds = %d, want 1", got)
	}
	if got := client.created[0].AutoDeleteSeconds; got != 1 {
		t.Fatalf("AutoDeleteSeconds = %d, want 1", got)
	}
}

func TestRunnerCancellationStopsDispatchAndBestEffortCleanup(t *testing.T) {
	client := &fakeLoadClient{createDelay: time.Millisecond}
	cfg := testRunnerConfig()
	cfg.Operations = 100
	cfg.Concurrency = 2
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(5 * time.Millisecond); cancel() }()
	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: metrics.NoopProvider{}}, Clock: time.Now}).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context canceled", err)
	}
	if summary.Started >= int64(cfg.Operations) {
		t.Fatalf("cancellation did not stop dispatch: %+v", summary)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.deleted) != int(summary.Started) {
		t.Fatalf("cleanup count %d, started %d", len(client.deleted), summary.Started)
	}
}

func TestRunnerReturnsFailureButAttemptsDeleteWhenCleanupFails(t *testing.T) {
	client := &fakeLoadClient{deleteErr: errors.New("delete unavailable")}
	cfg := testRunnerConfig()
	cfg.Operations = 1
	cfg.Concurrency = 1
	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: metrics.NoopProvider{}}, Clock: time.Now}).Run(context.Background())
	if err == nil || summary.Failed != 1 || summary.Completed != 0 {
		t.Fatalf("unexpected result: summary=%+v err=%v", summary, err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.created) != 1 || len(client.deleted) != 1 {
		t.Fatalf("cleanup was not attempted: created=%d deleted=%d", len(client.created), len(client.deleted))
	}
}

func TestRunnerReturnsWaitActiveFailureAndDeletesCreatedSandbox(t *testing.T) {
	client := &fakeLoadClient{status: "creating"}
	cfg := testRunnerConfig()
	cfg.Operations = 1
	cfg.Concurrency = 1
	cfg.ActiveTimeout = 5 * time.Millisecond
	cfg.PollInterval = time.Millisecond

	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: metrics.NoopProvider{}}, Clock: time.Now}).Run(context.Background())
	if err == nil || summary.Started != 1 || summary.Failed != 1 || summary.Completed != 0 {
		t.Fatalf("unexpected wait-active failure result: summary=%+v err=%v", summary, err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.created) != 1 || len(client.deleted) != 1 {
		t.Fatalf("created=%d deleted=%d, want one of each", len(client.created), len(client.deleted))
	}
}

func TestRunnerRecordsFailedVerifyExecWhenStdoutMismatch(t *testing.T) {
	client := &fakeLoadClient{execStdout: "unexpected"}
	metrics := &loadMetricsRecorder{}
	cfg := testRunnerConfig()
	cfg.Operations = 1
	cfg.Concurrency = 1

	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: metrics}, Clock: time.Now}).Run(context.Background())
	if err == nil || summary.Failed != 1 || summary.Completed != 0 {
		t.Fatalf("unexpected result: summary=%+v err=%v", summary, err)
	}
	if got := metrics.stepResult("verify_exec"); got != "failure" {
		t.Fatalf("verify_exec telemetry result = %q, want failure", got)
	}
}

type loadMetricsRecorder struct {
	mu    sync.Mutex
	steps map[string]string
}

func (r *loadMetricsRecorder) RecordRun(context.Context, string, string, string, string, string, time.Duration) {
}

func (r *loadMetricsRecorder) RecordStep(_ context.Context, _ string, _ string, _ string, _ string, step, result string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steps == nil {
		r.steps = make(map[string]string)
	}
	r.steps[step] = result
}

func (r *loadMetricsRecorder) RecordCleanup(context.Context, string, string, string, string) {}
func (r *loadMetricsRecorder) RecordOverlapSkip(context.Context, string, string, string)     {}
func (r *loadMetricsRecorder) RecordExecutionDelta(context.Context, string, string, string, string, int64) {
}
func (r *loadMetricsRecorder) RecordOrphans(context.Context, string, string, string, int64, time.Duration) {
}
func (r *loadMetricsRecorder) RecordRetainedSandbox(context.Context, string, string, string, string) {
}
func (r *loadMetricsRecorder) RecordJanitorResources(context.Context, string, string, string, int64, int64, int64) {
}

func (r *loadMetricsRecorder) stepResult(step string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.steps[step]
}

func TestSandboxNameForOperationIncludesWorkerIdentity(t *testing.T) {
	a := sandboxNameForOperation("run-1", "worker-1", 7)
	b := sandboxNameForOperation("run-1", "worker-2", 7)

	if a == b {
		t.Fatalf("expected distinct sandbox names, got %q", a)
	}
	if !strings.Contains(a, "worker-1") {
		t.Fatalf("sandbox name %q does not include worker identity", a)
	}
	if !strings.Contains(b, "worker-2") {
		t.Fatalf("sandbox name %q does not include worker identity", b)
	}
}
