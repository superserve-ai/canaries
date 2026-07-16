package lifecycle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
)

func TestSandboxName(t *testing.T) {
	got := sandboxName("staging-usc1", "run-123")
	if got != "api-canary-staging-usc1-run-123" {
		t.Fatalf("sandboxName = %q", got)
	}
}

func TestWaitForStatusTerminalFailure(t *testing.T) {
	client := &fakeClient{
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{Status: "failed"}, nil
		},
	}
	r := Runner{
		Config: config.Config{
			PollInterval:   time.Millisecond,
			CommandTimeout: 20 * time.Millisecond,
		},
		Client: client,
		Clock:  time.Now,
	}

	err := r.waitForStatus(context.Background(), "sb-1", "active")
	if err == nil || !strings.Contains(err.Error(), `sandbox entered terminal state "failed"`) {
		t.Fatalf("waitForStatus error = %v", err)
	}
}

func TestUploadVerificationUtilitiesUsesRepoAssets(t *testing.T) {
	var commands []string
	client := &fakeClient{
		execFn: func(_ context.Context, _ string, _ string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			commands = append(commands, req.Command)
			return canaryapi.ExecResult{ExitCode: 0}, nil
		},
	}
	r := Runner{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	if err := r.uploadVerificationUtilities(context.Background(), "sb-1", "tok"); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected one upload command, got %d", len(commands))
	}
	if !strings.Contains(commands[0], "verify_disk.sh") || !strings.Contains(commands[0], "verify_memory.py") {
		t.Fatalf("upload command did not reference verification assets: %s", commands[0])
	}
	if !strings.Contains(commands[0], "/tmp/verification-utilities") {
		t.Fatalf("upload command did not target verification utilities dir: %s", commands[0])
	}
}

func TestCleanupPreservesPrimaryFailure(t *testing.T) {
	deleteCalled := false
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{}, errors.New("exec failed")
		},
		deleteSandboxFn: func(context.Context, string) error {
			deleteCalled = true
			return errors.New("delete failed")
		},
		previewURLFn: func(string, int) string { return "https://example.test" },
	}
	r := Runner{
		Config: config.Config{
			Target:         "staging-us-central1",
			Environment:    "staging",
			Region:         "us-central1",
			ResourceTTL:    time.Hour,
			PollInterval:   time.Millisecond,
			CommandTimeout: 20 * time.Millisecond,
			DeleteTimeout:  20 * time.Millisecond,
			PreviewPort:    18080,
		},
		Client:  client,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	result := r.runLifecycle(context.Background(), "run-1")
	if result.Err == nil {
		t.Fatal("expected error")
	}
	if got := result.Err.Error(); !strings.Contains(got, "priming sandbox: running initial_command step: exec failed") {
		t.Fatalf("unexpected error %q", got)
	}
	if strings.Contains(result.Err.Error(), "cleanup") {
		t.Fatalf("cleanup should not replace primary error: %q", result.Err)
	}
	if !deleteCalled {
		t.Fatal("expected delete attempt")
	}
}

func TestRunLifecycleRecordsTotalDurations(t *testing.T) {
	statusCalls := 0
	metrics := &lifecycleMetricsRecorder{}
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			statusCalls++
			switch statusCalls {
			case 1:
				return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
			case 2:
				return canaryapi.Sandbox{ID: "sb-1", Status: "paused", AccessToken: "tok"}, nil
			default:
				return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
			}
		},
		previewURLFn: func(string, int) string { return "https://preview.example.test" },
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{ExitCode: 0}, nil
		},
	}
	r := Runner{
		Config: config.Config{
			Target:                 "staging-us-central1",
			Environment:            "staging",
			Region:                 "us-central1",
			ResourceTTL:            time.Hour,
			PollInterval:           time.Millisecond,
			CommandTimeout:         20 * time.Millisecond,
			DeleteTimeout:          20 * time.Millisecond,
			PreviewPort:            18080,
			PreviewTimeout:         20 * time.Millisecond,
			RetainFailedSandbox:    false,
			RetainFailedSandboxTTL: 2 * time.Hour,
		},
		Client:  client,
		Locker:  lock.NoopLock{},
		Metrics: metrics,
		Clock:   time.Now,
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("preview-run-1")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	if result := r.runLifecycle(context.Background(), "run-1"); result.Err != nil {
		t.Fatalf("runLifecycle returned %v", result.Err)
	}

	for _, step := range []string{"create_total", "pause_total", "resume_total"} {
		if !containsStep(metrics.steps, step) {
			t.Fatalf("expected step %q in %v", step, metrics.steps)
		}
	}
}

func TestFailedRunRetainsSandboxWhenEnabled(t *testing.T) {
	updateCalled := false
	deleteCalled := false
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{}, errors.New("exec failed")
		},
		updateSandboxFn: func(context.Context, string, canaryapi.UpdateSandboxRequest) error {
			updateCalled = true
			return nil
		},
		deleteSandboxFn: func(context.Context, string) error {
			deleteCalled = true
			return nil
		},
	}
	r := Runner{
		Config: config.Config{
			Target:                 "staging-us-central1",
			Environment:            "staging",
			Region:                 "us-central1",
			ResourceTTL:            time.Hour,
			RetainFailedSandbox:    true,
			RetainFailedSandboxTTL: 2 * time.Hour,
			PollInterval:           time.Millisecond,
			CommandTimeout:         20 * time.Millisecond,
			DeleteTimeout:          20 * time.Millisecond,
			PreviewPort:            18080,
		},
		Client:  client,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	result := r.runLifecycle(context.Background(), "run-1")
	if result.Err == nil {
		t.Fatal("expected error")
	}
	if !updateCalled {
		t.Fatal("expected retention metadata update")
	}
	if deleteCalled {
		t.Fatal("did not expect delete when retention is enabled")
	}
	if !strings.Contains(result.Err.Error(), "priming sandbox") {
		t.Fatalf("expected primary error, got %q", result.Err)
	}
}

func TestRetentionMetadataFailureDoesNotReplacePrimaryError(t *testing.T) {
	updateCalled := false
	deleteCalled := false
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{}, errors.New("exec failed")
		},
		updateSandboxFn: func(context.Context, string, canaryapi.UpdateSandboxRequest) error {
			updateCalled = true
			return errors.New("metadata update failed")
		},
		deleteSandboxFn: func(context.Context, string) error {
			deleteCalled = true
			return nil
		},
	}
	r := Runner{
		Config: config.Config{
			Target:                 "staging-us-central1",
			Environment:            "staging",
			Region:                 "us-central1",
			ResourceTTL:            time.Hour,
			RetainFailedSandbox:    true,
			RetainFailedSandboxTTL: 2 * time.Hour,
			PollInterval:           time.Millisecond,
			CommandTimeout:         20 * time.Millisecond,
			DeleteTimeout:          20 * time.Millisecond,
			PreviewPort:            18080,
		},
		Client:  client,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	result := r.runLifecycle(context.Background(), "run-1")
	if result.Err == nil {
		t.Fatal("expected error")
	}
	if !updateCalled {
		t.Fatal("expected retention update attempt")
	}
	if deleteCalled {
		t.Fatal("did not expect delete when retention metadata update fails")
	}
	if !strings.Contains(result.Err.Error(), "priming sandbox") {
		t.Fatalf("expected primary error, got %q", result.Err)
	}
}

func TestFailedRunDeletesSandboxWhenRetentionDisabled(t *testing.T) {
	updateCalled := false
	deleteCalled := false
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{}, errors.New("exec failed")
		},
		updateSandboxFn: func(context.Context, string, canaryapi.UpdateSandboxRequest) error {
			updateCalled = true
			return nil
		},
		deleteSandboxFn: func(context.Context, string) error {
			deleteCalled = true
			return nil
		},
	}
	r := Runner{
		Config: config.Config{
			Target:         "staging-us-central1",
			Environment:    "staging",
			Region:         "us-central1",
			ResourceTTL:    time.Hour,
			PollInterval:   time.Millisecond,
			CommandTimeout: 20 * time.Millisecond,
			DeleteTimeout:  20 * time.Millisecond,
			PreviewPort:    18080,
		},
		Client:  client,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	result := r.runLifecycle(context.Background(), "run-1")
	if result.Err == nil {
		t.Fatal("expected error")
	}
	if updateCalled {
		t.Fatal("did not expect retention metadata update")
	}
	if !deleteCalled {
		t.Fatal("expected delete when retention is disabled")
	}
}

func TestFailureBeforeSandboxCreationRetainsNothing(t *testing.T) {
	deleteCalled := false
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{}, errors.New("create failed")
		},
		deleteSandboxFn: func(context.Context, string) error {
			deleteCalled = true
			return nil
		},
	}
	r := Runner{
		Config: config.Config{
			Target:         "staging-us-central1",
			Environment:    "staging",
			Region:         "us-central1",
			ResourceTTL:    time.Hour,
			PollInterval:   time.Millisecond,
			CommandTimeout: 20 * time.Millisecond,
			DeleteTimeout:  20 * time.Millisecond,
			PreviewPort:    18080,
		},
		Client:  client,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	result := r.runLifecycle(context.Background(), "run-1")
	if result.Err == nil {
		t.Fatal("expected error")
	}
	if deleteCalled {
		t.Fatal("did not expect delete when sandbox was never created")
	}
}

func TestVerifyPreviewRequiresExactToken(t *testing.T) {
	client := &fakeClient{
		previewURLFn: func(string, int) string { return "https://preview.example.test" },
	}

	r := Runner{
		Config: config.Config{
			Target:      "prod-usw2",
			Environment: "production",
			Region:      "us-west2",
			PreviewPort: 18080,
		},
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
		HTTP: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("wrong-token")),
				}, nil
			}),
		},
	}

	err := r.verifyPreview(context.Background(), "sb-1", "expected-token")
	if err == nil || !strings.Contains(err.Error(), "preview response mismatch") {
		t.Fatalf("verifyPreview error = %v", err)
	}
}

func TestVerifyPreviewRetriesTransientSandboxUnreachable(t *testing.T) {
	t.Parallel()

	attempts := 0
	r := Runner{
		Config: config.Config{
			Target:         "prod-usw2",
			Environment:    "production",
			Region:         "us-west2",
			PreviewPort:    18080,
			PreviewTimeout: 200 * time.Millisecond,
			PollInterval:   10 * time.Millisecond,
		},
		Client: &fakeClient{
			previewURLFn: func(string, int) string { return "https://preview.example.test" },
		},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				attempts++
				if attempts == 1 {
					return &http.Response{
						StatusCode: http.StatusBadGateway,
						Body:       io.NopCloser(strings.NewReader("sandbox unreachable")),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("preview-token")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	if err := r.verifyPreview(context.Background(), "sb-1", "preview-token"); err != nil {
		t.Fatalf("verifyPreview returned %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestVerifyPreviewWrapsProxyUnreachableWithURL(t *testing.T) {
	t.Parallel()

	r := Runner{
		Config: config.Config{
			Target:         "prod-usw2",
			Environment:    "production",
			Region:         "us-west2",
			PreviewPort:    18080,
			PreviewTimeout: 20 * time.Millisecond,
			PollInterval:   5 * time.Millisecond,
		},
		Client: &fakeClient{
			previewURLFn: func(string, int) string { return "https://18080-sb-1.preview.example.test" },
		},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Body:       io.NopCloser(strings.NewReader("sandbox unreachable")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	err := r.verifyPreview(context.Background(), "sb-1", "preview-token")
	if err == nil {
		t.Fatal("expected error")
	}
	var stepErr StepError
	if !errors.As(err, &stepErr) || stepErr.Step != "check_preview_url" {
		t.Fatalf("expected check_preview_url step error, got %v", err)
	}
	if !strings.Contains(err.Error(), "verifying preview URL https://18080-sb-1.preview.example.test") {
		t.Fatalf("missing preview URL context: %v", err)
	}
	if !strings.Contains(err.Error(), "preview proxy could not reach the sandbox listener") {
		t.Fatalf("missing proxy context: %v", err)
	}
}

func TestVerifyPreviewCreationFailureRecordsResolveStep(t *testing.T) {
	t.Parallel()

	r := Runner{
		Config: config.Config{
			Target:         "prod-usw2",
			Environment:    "production",
			Region:         "us-west2",
			PreviewPort:    18080,
			PreviewTimeout: 20 * time.Millisecond,
			PollInterval:   5 * time.Millisecond,
		},
		Client: &fakeClient{
			previewURLFn: func(string, int) string { return "://bad-url" },
		},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	err := r.verifyPreview(context.Background(), "sb-1", "preview-token")
	if err == nil {
		t.Fatal("expected error")
	}
	var stepErr StepError
	if !errors.As(err, &stepErr) || stepErr.Step != "resolve_preview_url" {
		t.Fatalf("expected resolve_preview_url step error, got %v", err)
	}
}

func TestRunSkipsAlreadyRunning(t *testing.T) {
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			t.Fatal("scenario should not run")
			return canaryapi.Sandbox{}, nil
		},
	}
	r := Runner{
		Config: config.Config{
			Target:      "staging-us-central1",
			Environment: "staging",
			Region:      "us-central1",
			RunTimeout:  time.Second,
			LockTTL:     time.Minute,
		},
		Client:  client,
		Locker:  alreadyRunningLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

type lifecycleMetricsRecorder struct {
	steps []string
}

func (m *lifecycleMetricsRecorder) RecordRun(context.Context, string, string, string, string, string, time.Duration) {
}

func (m *lifecycleMetricsRecorder) RecordStep(_ context.Context, _, _, _, _, step, result string, _ time.Duration) {
	m.steps = append(m.steps, step+":"+result)
}

func (m *lifecycleMetricsRecorder) RecordCleanup(context.Context, string, string, string, string) {}

func (m *lifecycleMetricsRecorder) RecordOverlapSkip(context.Context, string, string, string) {}

func (m *lifecycleMetricsRecorder) RecordExecutionDelta(context.Context, string, string, string, string, int64) {
}

func (m *lifecycleMetricsRecorder) RecordOrphans(context.Context, string, string, string, int64, time.Duration) {
}

func (m *lifecycleMetricsRecorder) RecordRetainedSandbox(context.Context, string, string, string, string) {
}

func (m *lifecycleMetricsRecorder) RecordJanitorResources(context.Context, string, string, string, int64, int64, int64) {
}

func containsStep(steps []string, want string) bool {
	for _, step := range steps {
		if strings.HasPrefix(step, want+":") {
			return true
		}
	}
	return false
}

type alreadyRunningLock struct{}

func (alreadyRunningLock) Acquire(context.Context, string, time.Duration) (lock.Outcome, lock.Lease, error) {
	return lock.OutcomeAlreadyRunning, nil, nil
}

func (alreadyRunningLock) Release(context.Context) error {
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type fakeClient struct {
	createSandboxFn func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error)
	getSandboxFn    func(context.Context, string) (canaryapi.Sandbox, error)
	pauseSandboxFn  func(context.Context, string) error
	resumeSandboxFn func(context.Context, string) (canaryapi.ResumeResponse, error)
	deleteSandboxFn func(context.Context, string) error
	updateSandboxFn func(context.Context, string, canaryapi.UpdateSandboxRequest) error
	execFn          func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error)
	previewURLFn    func(string, int) string
}

func (f *fakeClient) CreateSandbox(ctx context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
	return f.createSandboxFn(ctx, req)
}

func (f *fakeClient) GetSandbox(ctx context.Context, id string) (canaryapi.Sandbox, error) {
	return f.getSandboxFn(ctx, id)
}

func (f *fakeClient) PauseSandbox(ctx context.Context, id string) error {
	if f.pauseSandboxFn == nil {
		return nil
	}
	return f.pauseSandboxFn(ctx, id)
}

func (f *fakeClient) ResumeSandbox(ctx context.Context, id string) (canaryapi.ResumeResponse, error) {
	if f.resumeSandboxFn == nil {
		return canaryapi.ResumeResponse{ID: id, Status: "active", AccessToken: "tok"}, nil
	}
	return f.resumeSandboxFn(ctx, id)
}

func (f *fakeClient) DeleteSandbox(ctx context.Context, id string) error {
	if f.deleteSandboxFn == nil {
		return nil
	}
	return f.deleteSandboxFn(ctx, id)
}

func (f *fakeClient) UpdateSandbox(ctx context.Context, id string, req canaryapi.UpdateSandboxRequest) error {
	if f.updateSandboxFn == nil {
		return nil
	}
	return f.updateSandboxFn(ctx, id, req)
}

func (f *fakeClient) Exec(ctx context.Context, sandboxID, accessToken string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
	return f.execFn(ctx, sandboxID, accessToken, req)
}

func (f *fakeClient) PreviewURL(sandboxID string, port int) string {
	return f.previewURLFn(sandboxID, port)
}
