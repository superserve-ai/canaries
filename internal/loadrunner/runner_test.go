package loadrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	mu           sync.Mutex
	created      []canaryapi.CreateSandboxRequest
	deleted      []string
	active       int
	maxActive    int
	deleteErr    error
	createDelay  time.Duration
	status       string
	execStdout   string
	execExitCode int
	execErr      error
	statusCalls  map[string]int
	waited       map[string]bool
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
	return canaryapi.ExecResult{Stdout: stdout, ExitCode: f.execExitCode}, f.execErr
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
	if summary.ConfiguredOperations != int64(cfg.Operations) || summary.ConfiguredConcurrency != int64(cfg.Concurrency) || summary.InFlight != 0 || summary.Unstarted != 0 || len(summary.Create.Durations) != cfg.Operations || len(summary.LifecycleDurations) != cfg.Operations || len(summary.Delete.Durations) != 0 || summary.Pause.Attempts != 0 || summary.Resume.Attempts != 0 {
		t.Fatalf("unexpected create-only aggregation: %+v", summary)
	}
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

func TestRunnerVerifyExec(t *testing.T) {
	transportErr := errors.New("exec unavailable")
	deleteErr := errors.New("delete unavailable")
	for _, tt := range []struct {
		name      string
		stdout    string
		exitCode  int
		execErr   error
		deleteErr error
		wantError string
	}{
		{name: "success", stdout: "superserve-load-test"},
		{name: "nonzero exit with expected stdout", stdout: "superserve-load-test", exitCode: 42, wantError: "exec command failed: exit=42"},
		{name: "stdout mismatch", stdout: "unexpected", wantError: `unexpected exec output "unexpected"`},
		{name: "transport error", execErr: transportErr, wantError: "exec unavailable"},
		{name: "canceled exec", execErr: context.Canceled, wantError: "context canceled"},
		{name: "nonzero exit and cleanup failure", exitCode: 42, deleteErr: deleteErr, wantError: "exec command failed: exit=42"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, runSummary := range []bool{false, true} {
				name := "lifecycle"
				if runSummary {
					name = "summary"
				}
				t.Run(name, func(t *testing.T) {
					client := &fakeLoadClient{execStdout: tt.stdout, execExitCode: tt.exitCode, execErr: tt.execErr, deleteErr: tt.deleteErr}
					recorder := &loadMetricsRecorder{}
					cfg := testRunnerConfig()
					cfg.Operations = 1
					cfg.Concurrency = 1
					runner := Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: recorder}}
					wantFailure := tt.wantError != ""
					if runSummary {
						summary, err := runner.Run(context.Background())
						want := Summary{Started: 1, Completed: 1}
						if wantFailure {
							want.Completed, want.Failed = 0, 1
						}
						if (err != nil) != wantFailure || summary.Started != want.Started || summary.Completed != want.Completed || summary.Failed != want.Failed || summary.InFlight != want.InFlight {
							t.Fatalf("Run = (%+v, %v), want summary %+v, failure %v", summary, err, want, wantFailure)
						}
					} else {
						result, err := runner.runOne(context.Background(), 0)
						if wantFailure {
							var failure *lifecycleFailure
							if !errors.As(err, &failure) || failure.stage != "verify_exec" || failure.sandboxID != result.SandboxID || failure.Error() != tt.wantError {
								t.Fatalf("unexpected verification failure: result=%+v err=%v", result, err)
							}
							for _, cause := range []error{tt.execErr, tt.deleteErr} {
								if cause != nil && !errors.Is(err, cause) {
									t.Fatalf("error %v does not preserve cause %v", err, cause)
								}
							}
						} else if err != nil {
							t.Fatalf("runOne returned %v", err)
						}
					}
					if len(client.created) != 1 || len(client.deleted) != 1 || client.deleted[0] != client.created[0].Name {
						t.Fatalf("cleanup did not delete the created sandbox: created=%v deleted=%v", client.created, client.deleted)
					}
					wantResult := "success"
					if wantFailure {
						wantResult = "failure"
					}
					if got := recorder.stepResults("verify_exec"); len(got) != 1 || got[0] != wantResult {
						t.Fatalf("verify_exec telemetry = %v, want exactly one %s", got, wantResult)
					}
				})
			}
		})
	}
}

type loadMetricsRecorder struct {
	mu    sync.Mutex
	steps map[string][]string
}

func (r *loadMetricsRecorder) RecordRun(context.Context, string, string, string, string, string, time.Duration) {
}

func (r *loadMetricsRecorder) RecordStep(_ context.Context, _ string, _ string, _ string, _ string, step, result string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steps == nil {
		r.steps = make(map[string][]string)
	}
	r.steps[step] = append(r.steps[step], result)
}

func (r *loadMetricsRecorder) RecordCleanup(context.Context, string, string, string, string)     {}
func (r *loadMetricsRecorder) RecordOverlapSkip(context.Context, string, string, string, string) {}
func (r *loadMetricsRecorder) RecordExecutionDelta(context.Context, string, string, string, string, int64) {
}
func (r *loadMetricsRecorder) RecordOrphans(context.Context, string, string, string, int64, time.Duration) {
}
func (r *loadMetricsRecorder) RecordRetainedSandbox(context.Context, string, string, string, string, string) {
}
func (r *loadMetricsRecorder) RecordJanitorResources(context.Context, string, string, string, int64, int64, int64) {
}

func (r *loadMetricsRecorder) stepResults(step string) []string {
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

func (f *fakeLoadClient) CreateTemplate(context.Context, canaryapi.CreateTemplateRequest) (canaryapi.Template, error) {
	return canaryapi.Template{}, errors.New("not used")
}

func (f *fakeLoadClient) GetTemplateBuild(context.Context, string, string) (canaryapi.TemplateBuild, error) {
	return canaryapi.TemplateBuild{}, errors.New("not used")
}

func (f *fakeLoadClient) DeleteTemplate(context.Context, string) error { return errors.New("not used") }

func (f *fakeLoadClient) CreateSnapshot(context.Context, string, canaryapi.CreateSnapshotRequest) (canaryapi.Snapshot, error) {
	return canaryapi.Snapshot{}, errors.New("not used")
}

func (f *fakeLoadClient) GetSnapshot(context.Context, string) (canaryapi.Snapshot, error) {
	return canaryapi.Snapshot{}, errors.New("not used")
}

func (f *fakeLoadClient) DeleteSnapshot(context.Context, string) error { return errors.New("not used") }

func (f *fakeLoadClient) ListTemplates(context.Context, map[string]string) ([]canaryapi.Template, error) {
	return nil, errors.New("not used")
}

// lifecycleLoadClient models asynchronous transitions and rotates credentials.
// active counts sandboxes through terminal deletion, rather than DELETE ACK.
type lifecycleLoadClient struct {
	fakeLoadClient
	states         map[string]*loadSandboxState
	failureStage   string
	deletePolls    int
	deleteNotFound bool
	deleteNever    bool
	execTokens     []string
	cleanupContext []error
	onCreate       func(string)
	onExec         func(context.Context, string)
}

type loadSandboxState struct {
	phase       string
	polls       int
	execs       int
	deleted     bool
	deletePolls int
}

func (f *lifecycleLoadClient) CreateSandbox(ctx context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
	if f.failureStage == "create_request" {
		return canaryapi.Sandbox{}, errors.New("create unavailable")
	}
	sb, err := f.fakeLoadClient.CreateSandbox(ctx, req)
	f.mu.Lock()
	if f.states == nil {
		f.states = make(map[string]*loadSandboxState)
	}
	f.states[sb.ID] = &loadSandboxState{phase: "creating"}
	f.mu.Unlock()
	if f.onCreate != nil {
		f.onCreate(sb.ID)
	}
	return sb, err
}

func (f *lifecycleLoadClient) GetSandbox(_ context.Context, id string) (canaryapi.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.states[id]
	if s == nil {
		return canaryapi.Sandbox{}, canaryapi.ErrNotFound
	}
	f.statusCalls[id]++
	s.polls++
	status := s.phase
	switch s.phase {
	case "creating":
		if f.failureStage == "wait_active" {
			status = "failed"
		} else if s.polls > 1 {
			status, s.phase = "active", "active"
			f.waited[id] = true
		}
	case "pausing":
		if f.failureStage == "pause_wait_paused" {
			status = "failed"
		} else if s.polls > 1 {
			status, s.phase = "paused", "paused"
		}
	case "resuming":
		if f.failureStage == "resume_wait_active" {
			status = "failed"
		} else if s.polls > 1 {
			status, s.phase = "active", "active"
		}
	case "deleting":
		s.deletePolls++
		if !f.deleteNever && s.deletePolls > f.deletePolls {
			if !s.deleted {
				f.active--
				s.deleted = true
			}
			if f.deleteNotFound {
				return canaryapi.Sandbox{}, fmt.Errorf("fetch sandbox: %w", canaryapi.ErrNotFound)
			}
			status = "deleted"
		}
	}
	return canaryapi.Sandbox{ID: id, Status: status}, nil
}

func (f *lifecycleLoadClient) PauseSandbox(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failureStage == "pause_request" {
		return errors.New("pause unavailable")
	}
	f.states[id].phase, f.states[id].polls = "pausing", 0
	return nil
}

func (f *lifecycleLoadClient) ResumeSandbox(_ context.Context, id string) (canaryapi.ResumeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failureStage == "resume_request" {
		return canaryapi.ResumeResponse{}, errors.New("resume unavailable")
	}
	f.states[id].phase, f.states[id].polls = "resuming", 0
	return canaryapi.ResumeResponse{Status: "active", AccessToken: "fresh-resume-token"}, nil
}

func (f *lifecycleLoadClient) Exec(ctx context.Context, id, token string, _ canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
	f.mu.Lock()
	s := f.states[id]
	s.execs++
	execNumber := s.execs
	phase := s.phase
	f.execTokens = append(f.execTokens, token)
	f.mu.Unlock()
	if phase != "active" {
		return canaryapi.ExecResult{}, errors.New("exec before active")
	}
	if execNumber == 1 && token != "token" || execNumber == 2 && token != "fresh-resume-token" {
		return canaryapi.ExecResult{}, errors.New("stale credentials")
	}
	if f.onExec != nil {
		f.onExec(ctx, id)
	}
	if execNumber == 1 && f.failureStage == "verify_exec" || execNumber == 2 && f.failureStage == "verify_exec_after_resume" {
		return canaryapi.ExecResult{}, errors.New("verification unavailable")
	}
	return canaryapi.ExecResult{Stdout: "superserve-load-test"}, ctx.Err()
}

func (f *lifecycleLoadClient) DeleteSandbox(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	f.cleanupContext = append(f.cleanupContext, ctx.Err())
	if f.failureStage == "delete_request" {
		return errors.New("delete unavailable")
	}
	f.states[id].phase, f.states[id].polls = "deleting", 0
	return nil
}

func lifecycleRunnerConfig() Config {
	cfg := testRunnerConfig()
	cfg.Scenario = "lifecycle"
	cfg.Operations, cfg.Concurrency = 1, 1
	return cfg
}

func TestRunnerFullLifecycleUsesFreshCredentialsAndWaitsForDelete(t *testing.T) {
	for _, notFound := range []bool{false, true} {
		t.Run(fmt.Sprintf("not_found_%v", notFound), func(t *testing.T) {
			client := &lifecycleLoadClient{deletePolls: 2, deleteNotFound: notFound}
			cfg := lifecycleRunnerConfig()
			summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
			if err != nil || summary.Completed != 1 || summary.InFlight != 0 || summary.Unstarted != 0 {
				t.Fatalf("Run = (%+v, %v)", summary, err)
			}
			for name, phase := range map[string]PhaseSummary{"create": summary.Create, "pause": summary.Pause, "resume": summary.Resume, "delete": summary.Delete} {
				if phase.Attempts != 1 || phase.Failures != 0 || len(phase.Durations) != 1 {
					t.Errorf("%s phase = %+v", name, phase)
				}
			}
			if len(summary.LifecycleDurations) != 1 || summary.Delete.Durations[0] < 2*cfg.PollInterval {
				t.Fatalf("did not measure completion polling: %+v", summary)
			}
			if got := strings.Join(client.execTokens, ","); got != "token,fresh-resume-token" {
				t.Fatalf("exec tokens = %q", got)
			}
			if client.active != 0 || len(client.deleted) != 1 || client.states[client.deleted[0]].deletePolls != 3 {
				t.Fatalf("delete did not reach terminal state: active=%d deleted=%v", client.active, client.deleted)
			}
		})
	}
}

func TestRunnerSuccessfulPhaseSamplesSurviveLaterFailures(t *testing.T) {
	for _, tt := range []struct {
		stage          string
		attempts       [4]int64
		failures       [4]int64
		samples        [4]int
		cleanupFailure bool
	}{
		{stage: "create_request", attempts: [4]int64{1, 0, 0, 0}, failures: [4]int64{1, 0, 0, 0}},
		{stage: "wait_active", attempts: [4]int64{1, 0, 0, 1}, failures: [4]int64{1, 0, 0, 0}, samples: [4]int{0, 0, 0, 1}},
		{stage: "verify_exec", attempts: [4]int64{1, 0, 0, 1}, samples: [4]int{1, 0, 0, 1}},
		{stage: "pause_request", attempts: [4]int64{1, 1, 0, 1}, failures: [4]int64{0, 1, 0, 0}, samples: [4]int{1, 0, 0, 1}},
		{stage: "pause_wait_paused", attempts: [4]int64{1, 1, 0, 1}, failures: [4]int64{0, 1, 0, 0}, samples: [4]int{1, 0, 0, 1}},
		{stage: "resume_request", attempts: [4]int64{1, 1, 1, 1}, failures: [4]int64{0, 0, 1, 0}, samples: [4]int{1, 1, 0, 1}},
		{stage: "resume_wait_active", attempts: [4]int64{1, 1, 1, 1}, failures: [4]int64{0, 0, 1, 0}, samples: [4]int{1, 1, 0, 1}},
		{stage: "verify_exec_after_resume", attempts: [4]int64{1, 1, 1, 1}, samples: [4]int{1, 1, 1, 1}},
		{stage: "delete_request", attempts: [4]int64{1, 1, 1, 1}, failures: [4]int64{0, 0, 0, 1}, samples: [4]int{1, 1, 1, 0}, cleanupFailure: true},
		{stage: "delete_wait_deleted", attempts: [4]int64{1, 1, 1, 1}, failures: [4]int64{0, 0, 0, 1}, samples: [4]int{1, 1, 1, 0}, cleanupFailure: true},
	} {
		t.Run(tt.stage, func(t *testing.T) {
			client := &lifecycleLoadClient{failureStage: tt.stage, deleteNever: tt.stage == "delete_wait_deleted"}
			cfg := lifecycleRunnerConfig()
			cfg.DeleteTimeout = 5 * time.Millisecond
			summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
			if err == nil || summary.Failed != 1 || summary.Completed != 0 || len(summary.LifecycleDurations) != 0 || summary.FailurePhases[tt.stage] != 1 {
				t.Fatalf("Run = (%+v, %v)", summary, err)
			}
			if got := summary.CleanupFailures != 0; got != tt.cleanupFailure {
				t.Errorf("CleanupFailures = %d, want failure %v", summary.CleanupFailures, tt.cleanupFailure)
			}
			for i, phase := range []PhaseSummary{summary.Create, summary.Pause, summary.Resume, summary.Delete} {
				if phase.Attempts != tt.attempts[i] || phase.Failures != tt.failures[i] || len(phase.Durations) != tt.samples[i] {
					t.Errorf("phase %d = %+v, want attempts=%d failures=%d samples=%d", i, phase, tt.attempts[i], tt.failures[i], tt.samples[i])
				}
			}
		})
	}
}

func TestRunnerLifecycleConcurrencyIncludesDeletionCompletion(t *testing.T) {
	client := &lifecycleLoadClient{deletePolls: 2}
	cfg := lifecycleRunnerConfig()
	cfg.Operations, cfg.Concurrency = 20, 4
	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
	if err != nil || summary.Completed != int64(cfg.Operations) || summary.InFlight != 0 || summary.PeakInFlight != int64(cfg.Concurrency) {
		t.Fatalf("Run = (%+v, %v)", summary, err)
	}
	if client.maxActive > cfg.Concurrency || client.active != 0 {
		t.Fatalf("outstanding sandboxes max=%d active=%d configured=%d", client.maxActive, client.active, cfg.Concurrency)
	}
}

func TestRunnerCleanupFailureStopsReplacementDispatch(t *testing.T) {
	for _, stage := range []string{"delete_request", "delete_wait_deleted"} {
		t.Run(stage, func(t *testing.T) {
			client := &lifecycleLoadClient{failureStage: stage, deleteNever: stage == "delete_wait_deleted"}
			cfg := lifecycleRunnerConfig()
			cfg.Operations, cfg.Concurrency = 100, 3
			cfg.DeleteTimeout = 5 * time.Millisecond
			summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
			if err == nil || summary.Started > int64(cfg.Concurrency) || summary.Started == 0 || summary.Unstarted != int64(cfg.Operations)-summary.Started || summary.InFlight != 0 || summary.CleanupFailures != summary.Started {
				t.Fatalf("cleanup failures did not halt replacement dispatch: summary=%+v err=%v", summary, err)
			}
			if len(client.deleted) != int(summary.Started) {
				t.Fatalf("cleanup attempts=%d started=%d", len(client.deleted), summary.Started)
			}
		})
	}
}

func TestRunnerCanceledLifecycleCleansIndependentlyAndCountsUnstarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &lifecycleLoadClient{deletePolls: 1}
	client.onExec = func(execCtx context.Context, _ string) {
		cancel()
		<-execCtx.Done()
	}
	cfg := lifecycleRunnerConfig()
	cfg.Operations = 100
	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(ctx)
	if !errors.Is(err, context.Canceled) || summary.Started != 1 || summary.Failed != 1 || summary.Unstarted != 99 || summary.InFlight != 0 || summary.CleanupFailures != 0 {
		t.Fatalf("Run = (%+v, %v)", summary, err)
	}
	if client.active != 0 || len(client.deleted) != 1 || len(client.cleanupContext) != 1 || client.cleanupContext[0] != nil {
		t.Fatalf("cleanup did not outlive canceled run: active=%d deleted=%v context=%v", client.active, client.deleted, client.cleanupContext)
	}
	if len(summary.Create.Durations) != 1 || len(summary.Delete.Durations) != 1 || summary.Pause.Attempts != 0 {
		t.Fatalf("incorrect samples for canceled operation: %+v", summary)
	}
}

func TestRunnerStaticClockCannotHangLifecycleCleanup(t *testing.T) {
	client := &lifecycleLoadClient{deleteNever: true}
	cfg := lifecycleRunnerConfig()
	cfg.DeleteTimeout = 3 * time.Millisecond
	fixedClock := func() time.Time { return time.Unix(100, 0) }
	summary, err := (Runner{Config: cfg, Clock: fixedClock, Ops: lifecycle.Operations{Client: client, Clock: fixedClock}}).Run(context.Background())
	if err == nil || summary.FailurePhases["delete_wait_deleted"] != 1 || summary.CleanupFailures != 1 || len(summary.Delete.Durations) != 0 {
		t.Fatalf("Run = (%+v, %v)", summary, err)
	}
	if len(summary.Create.Durations) != 1 || summary.Create.Durations[0] != 0 {
		t.Fatalf("successful zero-duration samples should be retained: %+v", summary.Create)
	}
}

func TestRunnerRejectsOperationCountsBeyondSampleBound(t *testing.T) {
	client := &fakeLoadClient{}
	cfg := testRunnerConfig()
	cfg.Operations = MaxOperations + 1
	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
	if err == nil || summary.Started != 0 || len(client.created) != 0 {
		t.Fatalf("unbounded run accepted: summary=%+v err=%v", summary, err)
	}
}

func TestRunnerStaticClockCannotHangActivationWait(t *testing.T) {
	client := &fakeLoadClient{status: "creating"}
	cfg := testRunnerConfig()
	cfg.Operations, cfg.Concurrency = 1, 1
	cfg.ActiveTimeout = 3 * time.Millisecond
	fixedClock := func() time.Time { return time.Unix(100, 0) }
	summary, err := (Runner{Config: cfg, Clock: fixedClock, Ops: lifecycle.Operations{Client: client, Clock: fixedClock}}).Run(context.Background())
	if err == nil || summary.FailurePhases["wait_active"] != 1 || summary.Create.Failures != 1 || len(summary.Create.Durations) != 0 || len(client.deleted) != 1 {
		t.Fatalf("Run = (%+v, %v)", summary, err)
	}
}

type createOutcomeClient struct {
	fakeLoadClient
	createErr     error
	remoteCreated bool
}

func (f *createOutcomeClient) CreateSandbox(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	if f.remoteCreated {
		// The server committed creation, but the caller never receives an ID.
		f.active++
		f.maxActive = max(f.maxActive, f.active)
	}
	return canaryapi.Sandbox{}, f.createErr
}

func TestRunnerUncertainCreateStopsUnsafeReplacements(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"lost response", io.ErrUnexpectedEOF},
		{"transport failure", errors.New("connection lost after POST")},
		{"request timeout", context.DeadlineExceeded},
		{"HTTP request timeout", &canaryapi.HTTPStatusError{StatusCode: http.StatusRequestTimeout}},
		{"HTTP internal failure", &canaryapi.HTTPStatusError{StatusCode: http.StatusInternalServerError}},
		{"HTTP unavailable", &canaryapi.HTTPStatusError{StatusCode: http.StatusServiceUnavailable}},
		{"undecodable success response", &json.SyntaxError{Offset: 1}},
		{"missing ID success response", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &createOutcomeClient{createErr: tt.err, remoteCreated: true}
			cfg := testRunnerConfig()
			cfg.Operations, cfg.Concurrency = 100, 1
			summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
			if err == nil || summary.Started != 1 || summary.Failed != 1 || summary.Unstarted != 99 || summary.InFlight != 0 || summary.CleanupFailures != 0 || summary.FailurePhases["create_request"] != 1 {
				t.Fatalf("unsafe create outcome admitted replacements: summary=%+v err=%v", summary, err)
			}
			if len(client.created) != 1 || client.active != 1 || len(client.deleted) != 0 || summary.Delete.Attempts != 0 || len(summary.Create.Durations) != 0 {
				t.Fatalf("unknown creation incorrectly cleaned or replaced: requests=%d outstanding=%d deleted=%d summary=%+v", len(client.created), client.active, len(client.deleted), summary)
			}
		})
	}
}

func TestRunnerDefiniteCreateRejectionsContinueCollectingResults(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests} {
		t.Run(fmt.Sprintf("HTTP_%d", status), func(t *testing.T) {
			client := &createOutcomeClient{createErr: &canaryapi.HTTPStatusError{StatusCode: status}}
			cfg := testRunnerConfig()
			cfg.Operations, cfg.Concurrency = 8, 1
			summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
			if err == nil || summary.Started != int64(cfg.Operations) || summary.Failed != int64(cfg.Operations) || summary.Unstarted != 0 || summary.CleanupFailures != 0 || summary.FailurePhases["create_request"] != int64(cfg.Operations) {
				t.Fatalf("definite rejection stopped result collection: summary=%+v err=%v", summary, err)
			}
			if len(client.created) != cfg.Operations || client.active != 0 || len(client.deleted) != 0 || summary.Create.Failures != int64(cfg.Operations) {
				t.Fatalf("unexpected definite rejection accounting: requests=%d active=%d summary=%+v", len(client.created), client.active, summary)
			}
		})
	}
}

func TestRunnerCreateNotFoundUsesDefiniteRejectionRepresentation(t *testing.T) {
	// The API client represents HTTP 404 with ErrNotFound, not HTTPStatusError.
	client := &createOutcomeClient{createErr: fmt.Errorf("POST /sandboxes: %w", canaryapi.ErrNotFound)}
	cfg := testRunnerConfig()
	cfg.Operations, cfg.Concurrency = 3, 1
	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
	if err == nil || summary.Started != int64(cfg.Operations) || summary.Unstarted != 0 || summary.CleanupFailures != 0 || len(client.created) != cfg.Operations {
		t.Fatalf("definite 404 representation stopped dispatch: summary=%+v err=%v", summary, err)
	}
}

func TestRunnerScheduledLifecycleCapsEachActiveSessionAndKeepsResourceExpiry(t *testing.T) {
	client := &lifecycleLoadClient{}
	cfg := lifecycleRunnerConfig()
	cfg.Scheduled = true
	cfg.RunTimeout = 20 * time.Minute
	cfg.DeleteTimeout = 2 * time.Minute
	cfg.ResourceTTL = 2 * time.Hour
	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
	if err != nil || summary.Completed != 1 {
		t.Fatalf("Run = (%+v, %v)", summary, err)
	}
	req := client.created[0]
	if req.TimeoutSeconds != 60 || req.TimeoutSeconds >= req.AutoDeleteSeconds {
		t.Fatalf("active-session timeout=%ds auto-delete=%ds, want a fixed 60s active-session cap", req.TimeoutSeconds, req.AutoDeleteSeconds)
	}
	if len(client.execTokens) != 2 || client.execTokens[1] != "fresh-resume-token" {
		t.Fatal("scheduled lifecycle did not complete pause/resume verification with the fixed session cap")
	}
	if req.AutoDeleteSeconds != 7200 || req.IdleTimeoutSeconds == nil || *req.IdleTimeoutSeconds != 60 {
		t.Fatalf("scheduled create auto-delete=%d idle=%v, want 2h expiry and 60s idle pause", req.AutoDeleteSeconds, req.IdleTimeoutSeconds)
	}
	createdAt, parseErr := time.Parse(time.RFC3339, req.Metadata[sandboxmetadata.KeyCreatedAt])
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	expiresAt, parseErr := time.Parse(time.RFC3339, req.Metadata[sandboxmetadata.KeyExpiresAt])
	if parseErr != nil || expiresAt.Sub(createdAt) != cfg.ResourceTTL {
		t.Fatalf("janitor metadata expiry changed: created=%s expires=%s error=%v", createdAt, expiresAt, parseErr)
	}
}

func TestRunnerManualCreateKeepsConfiguredComputeTimeoutAndNoIdleOverride(t *testing.T) {
	client := &fakeLoadClient{}
	cfg := testRunnerConfig()
	cfg.Operations, cfg.Concurrency = 1, 1
	summary, err := (Runner{Config: cfg, Ops: lifecycle.Operations{Client: client}}).Run(context.Background())
	if err != nil || summary.Completed != 1 {
		t.Fatalf("Run = (%+v, %v)", summary, err)
	}
	req := client.created[0]
	if req.TimeoutSeconds != int(cfg.ResourceTTL/time.Second) || req.AutoDeleteSeconds != req.TimeoutSeconds || req.IdleTimeoutSeconds != nil {
		t.Fatalf("manual create timeout semantics changed: timeout=%d auto-delete=%d idle=%v", req.TimeoutSeconds, req.AutoDeleteSeconds, req.IdleTimeoutSeconds)
	}
}
