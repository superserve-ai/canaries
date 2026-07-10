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
	if err == nil || err.Error() != `terminal state "failed"` {
		t.Fatalf("waitForStatus error = %v", err)
	}
}

func TestCleanupPreservesPrimaryFailure(t *testing.T) {
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
		Locker:  lock.NoopLocker{},
		Metrics: metrics.Provider{},
		Clock:   time.Now,
	}

	err := r.runLifecycle(context.Background(), "run-1")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "exec failed; cleanup: delete failed" {
		t.Fatalf("unexpected error %q", got)
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
		Metrics: metrics.Provider{},
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
	if err == nil || err.Error() != "preview response mismatch" {
		t.Fatalf("verifyPreview error = %v", err)
	}
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

func (f *fakeClient) Exec(ctx context.Context, sandboxID, accessToken string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
	return f.execFn(ctx, sandboxID, accessToken, req)
}

func (f *fakeClient) PreviewURL(sandboxID string, port int) string {
	return f.previewURLFn(sandboxID, port)
}
