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
	var writes []struct {
		path string
		size int
	}
	client := &fakeClient{
		writeFileFn: func(_ context.Context, sandboxID, _ string, path string, content []byte) error {
			if sandboxID != "sb-1" {
				t.Fatalf("unexpected sandboxID %q", sandboxID)
			}
			writes = append(writes, struct {
				path string
				size int
			}{path: path, size: len(content)})
			return nil
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
	if len(writes) != 2 {
		t.Fatalf("expected two writes, got %d", len(writes))
	}
	if writes[0].path != "/tmp/verification-utilities/verify_disk.sh" {
		t.Fatalf("first write path = %q", writes[0].path)
	}
	if writes[0].size == 0 || writes[1].size == 0 {
		t.Fatalf("expected non-empty verification assets: %+v", writes)
	}
	if writes[1].path != "/tmp/verification-utilities/verify_memory.py" {
		t.Fatalf("second write path = %q", writes[1].path)
	}
}

func TestWriteSandboxFileWithRetryRetriesTransientProxyErrors(t *testing.T) {
	attempts := 0
	client := &fakeClient{
		writeFileFn: func(context.Context, string, string, string, []byte) error {
			attempts++
			if attempts == 1 {
				return &canaryapi.HTTPStatusError{
					Method:     http.MethodPost,
					Path:       "/files",
					StatusCode: http.StatusServiceUnavailable,
					Body:       "proxy not ready",
				}
			}
			return nil
		},
	}
	r := Runner{
		Client: client,
	}

	if err := r.writeSandboxFileWithRetry(context.Background(), "sb-1", "tok", "/tmp/canary-token", []byte("token")); err != nil {
		t.Fatalf("writeSandboxFileWithRetry returned %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
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
	clock := &fakeClock{now: time.Unix(0, 0)}
	statusCalls := 0
	writeCalls := 0
	verificationUploadAdvanced := false
	metrics := &lifecycleMetricsRecorder{}
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			clock.Advance(2 * time.Second)
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			statusCalls++
			switch statusCalls {
			case 1:
				clock.Advance(3 * time.Second)
				return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
			case 2:
				clock.Advance(13 * time.Second)
				return canaryapi.Sandbox{ID: "sb-1", Status: "paused", AccessToken: "tok"}, nil
			default:
				clock.Advance(19 * time.Second)
				return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
			}
		},
		pauseSandboxFn: func(context.Context, string) error {
			clock.Advance(11 * time.Second)
			return nil
		},
		resumeSandboxFn: func(context.Context, string) (canaryapi.ResumeResponse, error) {
			clock.Advance(17 * time.Second)
			return canaryapi.ResumeResponse{ID: "sb-1", Status: "active", AccessToken: "resume-token"}, nil
		},
		publishPreviewPortFn: func(context.Context, string, canaryapi.PublishPreviewPortRequest) error {
			clock.Advance(39 * time.Second)
			return nil
		},
		previewURLFn: func(string, int) string { return "https://preview.example.test" },
		writeFileFn: func(_ context.Context, sandboxID, _ string, path string, _ []byte) error {
			if sandboxID != "sb-1" {
				t.Fatalf("unexpected sandboxID %q", sandboxID)
			}
			writeCalls++
			if strings.Contains(path, "verification-utilities") && !verificationUploadAdvanced {
				clock.Advance(23 * time.Second)
				verificationUploadAdvanced = true
			}
			return nil
		},
		execFn: func(_ context.Context, _ string, _ string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			switch {
			case strings.Contains(req.Command, "time.sleep(3600)"):
				clock.Advance(7 * time.Second)
			case strings.Contains(req.Command, "verify_disk.sh"):
				clock.Advance(29 * time.Second)
			case strings.Contains(req.Command, "verify_memory.py"):
				clock.Advance(31 * time.Second)
			case strings.Contains(req.Command, "http.server"):
				clock.Advance(37 * time.Second)
			default:
				t.Fatalf("unexpected exec command: %s", req.Command)
			}
			return canaryapi.ExecResult{ExitCode: 0}, nil
		},
		deleteSandboxFn: func(context.Context, string) error {
			clock.Advance(43 * time.Second)
			return nil
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
		Clock:   clock.Now,
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				clock.Advance(41 * time.Second)
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

	wantDurations := map[string]time.Duration{
		"create_request":                 2 * time.Second,
		"create_wait_active":             3 * time.Second,
		"create_total":                   5 * time.Second,
		"pause_request":                  11 * time.Second,
		"pause_wait_paused":              13 * time.Second,
		"pause_total":                    24 * time.Second,
		"resume_request":                 17 * time.Second,
		"resume_wait_active":             19 * time.Second,
		"resume_total":                   36 * time.Second,
		"prepare_verification_utilities": 23 * time.Second,
		"verify_disk":                    29 * time.Second,
		"verify_memory":                  31 * time.Second,
		"start_http_server":              37 * time.Second,
		"publish_preview_port":           39 * time.Second,
		"preview":                        41 * time.Second,
		"delete_request":                 43 * time.Second,
	}
	for step, want := range wantDurations {
		if got := metrics.durations[step]; got != want {
			t.Fatalf("%s duration = %s, want %s", step, got, want)
		}
	}
}

func TestRunLifecyclePublishesPreviewPortBeforeCheckingPreviewURL(t *testing.T) {
	order := []string{}
	status := "active"
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: status, AccessToken: "tok"}, nil
		},
		pauseSandboxFn: func(context.Context, string) error {
			status = "paused"
			return nil
		},
		resumeSandboxFn: func(context.Context, string) (canaryapi.ResumeResponse, error) {
			status = "active"
			return canaryapi.ResumeResponse{ID: "sb-1", Status: "active", AccessToken: "resume-token"}, nil
		},
		publishPreviewPortFn: func(context.Context, string, canaryapi.PublishPreviewPortRequest) error {
			order = append(order, "publish_preview_port")
			return nil
		},
		writeFileFn: func(context.Context, string, string, string, []byte) error { return nil },
		execFn: func(_ context.Context, _ string, _ string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			switch {
			case strings.Contains(req.Command, "time.sleep(3600)"):
				return canaryapi.ExecResult{ExitCode: 0}, nil
			case strings.Contains(req.Command, "http.server"):
				return canaryapi.ExecResult{ExitCode: 0}, nil
			case strings.Contains(req.Command, "verify_disk.sh"):
				return canaryapi.ExecResult{ExitCode: 0}, nil
			case strings.Contains(req.Command, "verify_memory.py"):
				return canaryapi.ExecResult{ExitCode: 0}, nil
			default:
				t.Fatalf("unexpected exec command: %s", req.Command)
				return canaryapi.ExecResult{}, nil
			}
		},
		previewURLFn: func(string, int) string {
			order = append(order, "preview_url")
			return "https://preview.example.test"
		},
		deleteSandboxFn: func(context.Context, string) error { return nil },
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
		Metrics: metrics.NoopProvider{},
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
	if len(order) != 2 {
		t.Fatalf("expected 2 calls, got %v", order)
	}
	if order[0] != "publish_preview_port" || order[1] != "preview_url" {
		t.Fatalf("unexpected call order %v", order)
	}
}

func TestRunLifecycleFailsWhenPreviewPortPublicationFails(t *testing.T) {
	status := "active"
	client := &fakeClient{
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: status, AccessToken: "tok"}, nil
		},
		pauseSandboxFn: func(context.Context, string) error {
			status = "paused"
			return nil
		},
		resumeSandboxFn: func(context.Context, string) (canaryapi.ResumeResponse, error) {
			status = "active"
			return canaryapi.ResumeResponse{ID: "sb-1", Status: "active", AccessToken: "resume-token"}, nil
		},
		publishPreviewPortFn: func(context.Context, string, canaryapi.PublishPreviewPortRequest) error {
			return errors.New("unexpected status 500")
		},
		writeFileFn: func(context.Context, string, string, string, []byte) error { return nil },
		execFn: func(_ context.Context, _ string, _ string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			switch {
			case strings.Contains(req.Command, "time.sleep(3600)"):
				return canaryapi.ExecResult{ExitCode: 0}, nil
			case strings.Contains(req.Command, "http.server"):
				return canaryapi.ExecResult{ExitCode: 0}, nil
			default:
				return canaryapi.ExecResult{ExitCode: 0}, nil
			}
		},
		deleteSandboxFn: func(context.Context, string) error { return nil },
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
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	result := r.runLifecycle(context.Background(), "run-1")
	if result.Err == nil {
		t.Fatal("expected error")
	}
	if result.FailedStep != "publish_preview_port" {
		t.Fatalf("failed step = %q", result.FailedStep)
	}
	if got := result.Err.Error(); !strings.Contains(got, "publishing preview port") {
		t.Fatalf("unexpected error %q", got)
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
	durations map[string]time.Duration
}

func (m *lifecycleMetricsRecorder) RecordRun(context.Context, string, string, string, string, string, time.Duration) {
}

func (m *lifecycleMetricsRecorder) RecordStep(_ context.Context, _, _, _, _, step, result string, duration time.Duration) {
	if m.durations == nil {
		m.durations = map[string]time.Duration{}
	}
	m.durations[step] = duration
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

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
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
	createSandboxFn      func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error)
	getSandboxFn         func(context.Context, string) (canaryapi.Sandbox, error)
	pauseSandboxFn       func(context.Context, string) error
	resumeSandboxFn      func(context.Context, string) (canaryapi.ResumeResponse, error)
	deleteSandboxFn      func(context.Context, string) error
	updateSandboxFn      func(context.Context, string, canaryapi.UpdateSandboxRequest) error
	writeFileFn          func(context.Context, string, string, string, []byte) error
	execFn               func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error)
	publishPreviewPortFn func(context.Context, string, canaryapi.PublishPreviewPortRequest) error
	previewURLFn         func(string, int) string
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

func (f *fakeClient) PublishPreviewPort(ctx context.Context, id string, req canaryapi.PublishPreviewPortRequest) error {
	if f.publishPreviewPortFn == nil {
		return nil
	}
	return f.publishPreviewPortFn(ctx, id, req)
}

func (f *fakeClient) WriteFile(ctx context.Context, sandboxID, accessToken, path string, content []byte) error {
	if f.writeFileFn == nil {
		return nil
	}
	return f.writeFileFn(ctx, sandboxID, accessToken, path, content)
}

func (f *fakeClient) Exec(ctx context.Context, sandboxID, accessToken string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
	return f.execFn(ctx, sandboxID, accessToken, req)
}

func (f *fakeClient) PreviewURL(sandboxID string, port int) string {
	return f.previewURLFn(sandboxID, port)
}
