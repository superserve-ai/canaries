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
	"github.com/superserve-ai/canaries/internal/metrics"
)

func TestCreateSandboxSetsJanitorMetadata(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	var gotReq canaryapi.CreateSandboxRequest
	client := &fakeClient{
		createSandboxFn: func(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			gotReq = req
			return canaryapi.Sandbox{ID: "sb-1", Status: "creating", AccessToken: "tok"}, nil
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   func() time.Time { return now },
	}

	req := canaryapi.CreateSandboxRequest{
		Name:              "custom-sandbox-name",
		FromTemplate:      "superserve/python-3.11",
		TimeoutSeconds:    240,
		AutoDeleteSeconds: 7200,
		Metadata: map[string]string{
			"managed_by": "load-runner",
			"run_id":     "run-123",
			"created_at": now.Format(time.RFC3339),
			"expires_at": now.Add(2 * time.Hour).Format(time.RFC3339),
		},
	}
	sb, err := ops.CreateSandbox(context.Background(), CreateSandboxOptions{
		Request: req,
		Telemetry: TelemetryContext{
			Environment: "staging",
			Region:      "us-central1",
			Target:      "staging-us-central1",
			Scenario:    "load-test",
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned %v", err)
	}
	if sb.ID != "sb-1" {
		t.Fatalf("sandbox id = %q", sb.ID)
	}
	if gotReq.Name != req.Name {
		t.Fatalf("sandbox name = %q", gotReq.Name)
	}
	if gotReq.Metadata["managed_by"] != "load-runner" {
		t.Fatalf("managed_by = %q", gotReq.Metadata["managed_by"])
	}
	if gotReq.Metadata["run_id"] != "run-123" {
		t.Fatalf("run_id = %q", gotReq.Metadata["run_id"])
	}
	if gotReq.Metadata["created_at"] != now.Format(time.RFC3339) {
		t.Fatalf("created_at = %q", gotReq.Metadata["created_at"])
	}
	if gotReq.Metadata["expires_at"] != now.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("expires_at = %q", gotReq.Metadata["expires_at"])
	}
}

func TestRunnerCreateSandboxBuildsCanaryRequest(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	var gotReq canaryapi.CreateSandboxRequest
	var getSandboxCalls int
	client := &fakeClient{
		createSandboxFn: func(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			gotReq = req
			return canaryapi.Sandbox{ID: "sb-1", Status: "creating", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			getSandboxCalls++
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
	}
	r := Runner{
		Config: config.Config{
			Target:                 "staging-us-central1",
			Environment:            "staging",
			Region:                 "us-central1",
			SandboxTemplate:        "superserve/python-3.11",
			RunTimeout:             4 * time.Minute,
			ResourceTTL:            time.Hour,
			RetainFailedSandbox:    true,
			RetainFailedSandboxTTL: 2 * time.Hour,
			PollInterval:           time.Millisecond,
			CommandTimeout:         20 * time.Millisecond,
		},
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   func() time.Time { return now },
	}

	resources, sb, err := r.CreateSandbox(context.Background(), "run-123")
	if err != nil {
		t.Fatalf("CreateSandbox returned %v", err)
	}
	if resources.SandboxID != "sb-1" || sb.ID != "sb-1" {
		t.Fatalf("unexpected sandbox ids: resources=%q sandbox=%q", resources.SandboxID, sb.ID)
	}
	if getSandboxCalls == 0 {
		t.Fatal("expected create shim to wait for active status")
	}
	if gotReq.Name != "api-canary-staging-us-central1-run-123" {
		t.Fatalf("sandbox name = %q", gotReq.Name)
	}
	if gotReq.FromTemplate != "superserve/python-3.11" {
		t.Fatalf("template = %q", gotReq.FromTemplate)
	}
	if gotReq.TimeoutSeconds != 240 {
		t.Fatalf("timeout seconds = %d", gotReq.TimeoutSeconds)
	}
	if gotReq.AutoDeleteSeconds != 7200 {
		t.Fatalf("auto delete seconds = %d", gotReq.AutoDeleteSeconds)
	}
	if gotReq.Metadata["managed_by"] != "api-canary" {
		t.Fatalf("managed_by = %q", gotReq.Metadata["managed_by"])
	}
	if gotReq.Metadata["expires_at"] != now.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("expires_at = %q", gotReq.Metadata["expires_at"])
	}
}

func TestRunnerCreateSandboxReturnsCreateWaitStepError(t *testing.T) {
	client := &fakeClient{
		createSandboxFn: func(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "creating", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "failed", AccessToken: "tok"}, nil
		},
	}
	r := Runner{
		Config: config.Config{
			Target:         "staging-us-central1",
			Environment:    "staging",
			Region:         "us-central1",
			PollInterval:   time.Millisecond,
			CommandTimeout: 20 * time.Millisecond,
		},
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	_, _, err := r.CreateSandbox(context.Background(), "run-123")
	if err == nil {
		t.Fatal("expected error")
	}
	var stepErr StepError
	if !errors.As(err, &stepErr) {
		t.Fatalf("expected StepError, got %T", err)
	}
	if stepErr.Step != "create_wait_active" {
		t.Fatalf("step = %q", stepErr.Step)
	}
	if !strings.Contains(err.Error(), `sandbox entered terminal state "failed"`) {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestRunnerCreateSandboxReturnsCreateRequestError(t *testing.T) {
	createErr := errors.New("create failed")
	client := &fakeClient{
		createSandboxFn: func(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{}, createErr
		},
	}
	r := Runner{
		Config: config.Config{
			Target:         "staging-us-central1",
			Environment:    "staging",
			Region:         "us-central1",
			PollInterval:   time.Millisecond,
			CommandTimeout: 20 * time.Millisecond,
		},
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	resources, sb, err := r.CreateSandbox(context.Background(), "run-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if resources.RunID != "run-123" {
		t.Fatalf("run id = %q", resources.RunID)
	}
	if resources.SandboxID != "" {
		t.Fatalf("expected no sandbox id, got %q", resources.SandboxID)
	}
	if sb.ID != "" {
		t.Fatalf("expected zero sandbox, got %+v", sb)
	}
	var stepErr StepError
	if errors.As(err, &stepErr) {
		t.Fatalf("did not expect StepError, got %+v", stepErr)
	}
	if !errors.Is(err, createErr) {
		t.Fatalf("expected wrapped create error, got %v", err)
	}
	if !strings.Contains(err.Error(), "creating sandbox") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestWaitForStatusTimedReturnsTerminalFailure(t *testing.T) {
	client := &fakeClient{
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "failed", AccessToken: "tok"}, nil
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	err := ops.WaitForStatusTimed(context.Background(), "sb-1", WaitForStatusOptions{
		Want:         "active",
		Timeout:      20 * time.Millisecond,
		PollInterval: time.Millisecond,
		Step:         "create_wait_active",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `sandbox entered terminal state "failed"`) {
		t.Fatalf("unexpected wait error %v", err)
	}
}

func TestWaitForStatusReturnsTimeout(t *testing.T) {
	client := &fakeClient{
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "creating", AccessToken: "tok"}, nil
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	err := ops.WaitForStatus(context.Background(), "sb-1", WaitForStatusOptions{
		Want:         "active",
		Timeout:      0,
		PollInterval: time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out waiting for sandbox to become active, last=creating") {
		t.Fatalf("unexpected wait error %v", err)
	}
}

func TestWaitForStatusReturnsContextCancellation(t *testing.T) {
	client := &fakeClient{
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "creating", AccessToken: "tok"}, nil
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ops.WaitForStatus(ctx, "sb-1", WaitForStatusOptions{
		Want:         "active",
		Timeout:      time.Second,
		PollInterval: time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !strings.Contains(err.Error(), `waiting for sandbox status "active": context canceled`) {
		t.Fatalf("unexpected wait error %v", err)
	}
}

func TestWaitForStatusOptionsDefaultPollInterval(t *testing.T) {
	if got := (WaitForStatusOptions{}).pollInterval(); got != 500*time.Millisecond {
		t.Fatalf("poll interval = %s", got)
	}
}

func TestWaitForStatusUsesDefaultPollInterval(t *testing.T) {
	calls := 0
	client := &fakeClient{
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			calls++
			if calls == 1 {
				return canaryapi.Sandbox{ID: "sb-1", Status: "creating", AccessToken: "tok"}, nil
			}
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	start := time.Now()
	err := ops.WaitForStatus(context.Background(), "sb-1", WaitForStatusOptions{
		Want:    "active",
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("WaitForStatus returned %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 status polls, got %d", calls)
	}
	if elapsed := time.Since(start); elapsed < 450*time.Millisecond {
		t.Fatalf("expected default poll interval delay, got %s", elapsed)
	}
}

func TestRunLifecycleDeletesSandboxAfterCreateWaitFailure(t *testing.T) {
	var deletedSandbox string
	client := &fakeClient{
		createSandboxFn: func(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "creating", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "failed", AccessToken: "tok"}, nil
		},
		deleteSandboxFn: func(_ context.Context, id string) error {
			deletedSandbox = id
			return nil
		},
	}
	r := Runner{
		Config: config.Config{
			Target:         "staging-us-central1",
			Environment:    "staging",
			Region:         "us-central1",
			ResourceTTL:    time.Hour,
			RunTimeout:     time.Minute,
			PollInterval:   time.Millisecond,
			CommandTimeout: 20 * time.Millisecond,
			DeleteTimeout:  time.Second,
		},
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	res := r.RunLifecycle(context.Background(), "run-123")
	if res.Err == nil {
		t.Fatal("expected error")
	}
	if res.SandboxID != "sb-1" {
		t.Fatalf("sandbox id = %q", res.SandboxID)
	}
	if deletedSandbox != "sb-1" {
		t.Fatalf("expected sandbox deletion, got %q", deletedSandbox)
	}
}

func TestOperationsFinalizeSandboxReturnsRetentionOutcome(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	var gotReq canaryapi.UpdateSandboxRequest
	client := &fakeClient{
		updateSandboxFn: func(_ context.Context, _ string, req canaryapi.UpdateSandboxRequest) error {
			gotReq = req
			return nil
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   func() time.Time { return now },
	}
	ttlSeconds := 7200
	primaryErr := errors.New("exec failed")

	outcome, err := ops.FinalizeSandbox(context.Background(), RunResources{
		SandboxID: "sb-1",
		RunID:     "run-123",
		CreatedAt: now.Add(-time.Minute),
	}, RunResult{
		Err:        primaryErr,
		FailedStep: "initial_command",
		SandboxID:  "sb-1",
	}, FinalizeOptions{
		Retain: RetentionOptions{
			Enabled: true,
			Metadata: map[string]string{
				"managed_by":         "api-canary",
				"retained_for_debug": "true",
				"failed_step":        "initial_command",
			},
			AutoDeleteSeconds: &ttlSeconds,
		},
		Telemetry: TelemetryContext{Scenario: "lifecycle"},
	})
	if !errors.Is(err, primaryErr) {
		t.Fatalf("expected primary error, got %v", err)
	}
	if !outcome.Retained {
		t.Fatal("expected retained outcome")
	}
	if outcome.RetentionError != nil || outcome.DeleteError != nil {
		t.Fatalf("unexpected finalize outcome: %+v", outcome)
	}
	if gotReq.AutoDeleteSeconds == nil || *gotReq.AutoDeleteSeconds != ttlSeconds {
		t.Fatalf("unexpected auto delete seconds: %+v", gotReq.AutoDeleteSeconds)
	}
	if gotReq.Metadata["retained_for_debug"] != "true" {
		t.Fatalf("unexpected retained metadata: %+v", gotReq.Metadata)
	}
}

func TestOperationsFinalizeSandboxReturnsRetentionErrorOutcome(t *testing.T) {
	primaryErr := errors.New("exec failed")
	retainErr := errors.New("update failed")
	client := &fakeClient{
		updateSandboxFn: func(context.Context, string, canaryapi.UpdateSandboxRequest) error {
			return retainErr
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	outcome, err := ops.FinalizeSandbox(context.Background(), RunResources{
		SandboxID: "sb-1",
		RunID:     "run-123",
		CreatedAt: time.Now(),
	}, RunResult{
		Err:        primaryErr,
		FailedStep: "initial_command",
		SandboxID:  "sb-1",
	}, FinalizeOptions{
		Retain: RetentionOptions{
			Enabled:  true,
			Metadata: map[string]string{"managed_by": "api-canary"},
		},
	})
	if !errors.Is(err, primaryErr) {
		t.Fatalf("expected primary error, got %v", err)
	}
	if outcome.Retained {
		t.Fatal("did not expect retained outcome")
	}
	if !errors.Is(outcome.RetentionError, retainErr) {
		t.Fatalf("expected retention error outcome, got %+v", outcome)
	}
}

func TestOperationsFinalizeSandboxReturnsDeleteErrorOutcome(t *testing.T) {
	deleteErr := errors.New("delete failed")
	client := &fakeClient{
		deleteSandboxFn: func(context.Context, string) error {
			return deleteErr
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	outcome, err := ops.FinalizeSandbox(context.Background(), RunResources{
		SandboxID: "sb-1",
		RunID:     "run-123",
		CreatedAt: time.Now(),
	}, RunResult{
		SandboxID: "sb-1",
	}, FinalizeOptions{
		Delete: DeleteSandboxOptions{
			Timeout: time.Second,
		},
	})
	if !errors.Is(err, deleteErr) {
		t.Fatalf("expected delete error, got %v", err)
	}
	if !errors.Is(outcome.DeleteError, deleteErr) {
		t.Fatalf("expected delete outcome error, got %+v", outcome)
	}
	if outcome.Retained || outcome.RetentionError != nil {
		t.Fatalf("unexpected finalize outcome: %+v", outcome)
	}
}

func TestOperationsFinalizeSandboxIgnoresDeleteNotFound(t *testing.T) {
	client := &fakeClient{
		deleteSandboxFn: func(context.Context, string) error {
			return canaryapi.ErrNotFound
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	outcome, err := ops.FinalizeSandbox(context.Background(), RunResources{
		SandboxID: "sb-1",
		RunID:     "run-123",
		CreatedAt: time.Now(),
	}, RunResult{
		SandboxID: "sb-1",
	}, FinalizeOptions{
		Delete: DeleteSandboxOptions{
			Timeout: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if outcome.DeleteError != nil || outcome.RetentionError != nil || outcome.Retained {
		t.Fatalf("unexpected finalize outcome: %+v", outcome)
	}
}

func TestRunnerFinalizeSandboxPropagatesDeleteErrorOnSuccess(t *testing.T) {
	deleteErr := errors.New("delete failed")
	client := &fakeClient{
		deleteSandboxFn: func(context.Context, string) error {
			return deleteErr
		},
	}
	r := Runner{
		Config: config.Config{
			Target:         "staging-us-central1",
			Environment:    "staging",
			Region:         "us-central1",
			DeleteTimeout:  time.Second,
			ResourceTTL:    time.Hour,
			CommandTimeout: time.Second,
		},
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	err := r.FinalizeSandbox(context.Background(), RunResources{
		SandboxID: "sb-1",
		RunID:     "run-123",
		CreatedAt: time.Now(),
	}, RunResult{
		SandboxID: "sb-1",
	})
	if !errors.Is(err, deleteErr) {
		t.Fatalf("expected delete error, got %v", err)
	}
}

func TestExecStepReturnsTypedValidationError(t *testing.T) {
	client := &fakeClient{
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{ExitCode: 42, Stderr: "boom"}, nil
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	_, err := ops.ExecStep(context.Background(), "sb-1", "tok", ExecStepOptions{
		Step:    "test_step",
		Command: "false",
		Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var validationErr ExecValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected ExecValidationError, got %v", err)
	}
	if validationErr.ExitCode != 42 {
		t.Fatalf("exit code = %d", validationErr.ExitCode)
	}
}

func TestExecReturnsTransportErrorWithoutValidationWrapping(t *testing.T) {
	transportErr := errors.New("upstream timeout")
	client := &fakeClient{
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{}, transportErr
		},
	}
	ops := Operations{Client: client}

	_, err := ops.Exec(context.Background(), "sb-1", "tok", canaryapi.ExecRequest{Command: "false", TimeoutS: 1})
	if err == nil {
		t.Fatal("expected error")
	}
	var validationErr ExecValidationError
	if errors.As(err, &validationErr) {
		t.Fatalf("did not expect validation error for transport failure: %v", err)
	}
	if !errors.Is(err, transportErr) {
		t.Fatalf("expected wrapped transport error, got %v", err)
	}
}

func TestDeleteSandboxBestEffortIgnoresNotFound(t *testing.T) {
	client := &fakeClient{
		deleteSandboxFn: func(context.Context, string) error {
			return canaryapi.ErrNotFound
		},
	}
	ops := Operations{
		Client:  client,
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	if err := ops.DeleteSandboxBestEffort(context.Background(), "sb-1", DeleteSandboxOptions{
		Timeout: time.Second,
	}); err != nil {
		t.Fatalf("DeleteSandboxBestEffort returned %v", err)
	}
}

func TestPublishPreviewPortUsesConfiguredPort(t *testing.T) {
	var gotReq canaryapi.PublishPreviewPortRequest
	client := &fakeClient{
		publishPreviewPortFn: func(_ context.Context, _ string, req canaryapi.PublishPreviewPortRequest) error {
			gotReq = req
			return nil
		},
	}
	ops := Operations{Client: client, Metrics: metrics.NoopProvider{}}

	if err := ops.PublishPreviewPort(context.Background(), "sb-1", PublishPreviewPortOptions{
		Port:   18080,
		Access: "public",
	}); err != nil {
		t.Fatalf("PublishPreviewPort returned %v", err)
	}
	if gotReq.Port != 18080 || gotReq.Access != "public" {
		t.Fatalf("unexpected publish request: %+v", gotReq)
	}
}

func TestVerifyPreviewUsesInjectedHTTPClient(t *testing.T) {
	called := false
	ops := Operations{
		Client: &fakeClient{
			previewURLFn: func(string, int) string { return "https://preview.example.test" },
		},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("preview-token")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	if err := ops.VerifyPreview(context.Background(), "sb-1", "preview-token", VerifyPreviewOptions{
		Port:         18080,
		Timeout:      20 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
	}); err != nil {
		t.Fatalf("VerifyPreview returned %v", err)
	}
	if !called {
		t.Fatal("expected injected http client to be used")
	}
}
