package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
)

func templateRunner(client *fakeClient) Runner {
	return Runner{
		Config: config.Config{
			Mode:                   config.ModeTemplate,
			Target:                 "staging-us-central1",
			Environment:            "staging",
			Region:                 "us-central1",
			TemplateBaseImage:      "ubuntu:22.04",
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

func TestRunTemplateBakesTokenLaunchesVerifiesAndCleansUpInOrder(t *testing.T) {
	var order []string
	var bakedToken string
	polls := 0
	client := &fakeClient{
		createTemplateFn: func(_ context.Context, req canaryapi.CreateTemplateRequest) (canaryapi.Template, error) {
			order = append(order, "create_template")
			if !strings.HasPrefix(req.Name, "api-canary-template-staging-us-central1-") || req.BuildSpec.From != "ubuntu:22.04" {
				t.Fatalf("unexpected template request %+v", req)
			}
			step := req.BuildSpec.Steps[0].Run
			bakedToken = step[strings.Index(step, "build-"):strings.Index(step, " > ")]
			return canaryapi.Template{ID: "tpl-1", Name: req.Name, BuildID: "build-1"}, nil
		},
		getTemplateBuildFn: func(_ context.Context, templateID, buildID string) (canaryapi.TemplateBuild, error) {
			polls++
			status := "building"
			if polls > 1 {
				status = "ready"
			}
			return canaryapi.TemplateBuild{ID: buildID, TemplateID: templateID, Status: status}, nil
		},
		createSandboxFn: func(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			order = append(order, "create_sandbox")
			if req.FromTemplate == "" || strings.HasPrefix(req.FromTemplate, "superserve/") {
				t.Fatalf("sandbox must launch from the built template, got %q", req.FromTemplate)
			}
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active"}, nil
		},
		execFn: func(_ context.Context, _ string, _ string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			order = append(order, "verify")
			if !strings.Contains(req.Command, buildTokenPath) || !strings.Contains(req.Command, bakedToken) {
				t.Fatalf("verify command must compare the baked token: %s", req.Command)
			}
			return canaryapi.ExecResult{ExitCode: 0}, nil
		},
		deleteSandboxFn: func(context.Context, string) error {
			order = append(order, "delete_sandbox")
			return nil
		},
		deleteTemplateFn: func(_ context.Context, id string) error {
			order = append(order, "delete_template")
			if id != "tpl-1" {
				t.Fatalf("delete template id = %q", id)
			}
			return nil
		},
	}

	if err := templateRunner(client).Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	want := []string{"create_template", "create_sandbox", "verify", "delete_sandbox", "delete_template"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if polls < 2 {
		t.Fatalf("expected the build to be polled until ready, polls = %d", polls)
	}
}

func TestRunTemplateBuildFailureReportsStepAndDeletesTemplate(t *testing.T) {
	templateDeleted := false
	client := &fakeClient{
		createTemplateFn: func(_ context.Context, req canaryapi.CreateTemplateRequest) (canaryapi.Template, error) {
			return canaryapi.Template{ID: "tpl-1", Name: req.Name, BuildID: "build-1"}, nil
		},
		getTemplateBuildFn: func(context.Context, string, string) (canaryapi.TemplateBuild, error) {
			return canaryapi.TemplateBuild{Status: "failed", ErrorMessage: "step_failed: exit status 1"}, nil
		},
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			t.Fatal("no sandbox should be created after a failed build")
			return canaryapi.Sandbox{}, nil
		},
		deleteTemplateFn: func(context.Context, string) error {
			templateDeleted = true
			return nil
		},
	}
	r := templateRunner(client)

	res := r.runTemplate(context.Background(), "run-1")
	if res.Err == nil || res.FailedStep != "template_build_wait" {
		t.Fatalf("failed step = %q err = %v", res.FailedStep, res.Err)
	}
	if res.BuildError != "step_failed: exit status 1" || res.TemplateID != "tpl-1" || res.BuildID != "build-1" {
		t.Fatalf("unexpected result context %+v", res)
	}
	if !templateDeleted {
		t.Fatal("template must be deleted when retention is off")
	}
}

// templatePollAttempts is retryTransient's budget per build status poll: one
// attempt plus four backoff delays (250ms, 500ms, 1s, 2s).
const templatePollAttempts = 5

// connectionReset mirrors the error the HTTP client returned in production
// when a build status poll was cut off mid read.
func connectionReset(op, path string) error {
	return &url.Error{
		Op:  op,
		URL: "https://api.example.test" + path,
		Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)},
	}
}

func buildPath(templateID, buildID string) string {
	return "/templates/" + templateID + "/builds/" + buildID
}

func templateOps(client *fakeClient) Operations {
	return Operations{Client: client, Metrics: metrics.NoopProvider{}, Clock: time.Now}
}

func createTemplateOK(_ context.Context, req canaryapi.CreateTemplateRequest) (canaryapi.Template, error) {
	return canaryapi.Template{ID: "tpl-1", Name: req.Name, BuildID: "build-1"}, nil
}

func TestRunTemplateRetriesBuildPollResetAndPasses(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() {
		log.Logger = orig
	})

	polls := 0
	sandboxCreated := false
	templateDeleted := false
	client := &fakeClient{
		createTemplateFn: createTemplateOK,
		getTemplateBuildFn: func(_ context.Context, templateID, buildID string) (canaryapi.TemplateBuild, error) {
			polls++
			switch polls {
			case 1:
				return canaryapi.TemplateBuild{ID: buildID, TemplateID: templateID, Status: "building"}, nil
			case 2:
				return canaryapi.TemplateBuild{}, connectionReset("Get", buildPath(templateID, buildID))
			default:
				return canaryapi.TemplateBuild{ID: buildID, TemplateID: templateID, Status: "ready"}, nil
			}
		},
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			sandboxCreated = true
			return canaryapi.Sandbox{ID: "sb-1", Status: "active", AccessToken: "tok"}, nil
		},
		getSandboxFn: func(context.Context, string) (canaryapi.Sandbox, error) {
			return canaryapi.Sandbox{ID: "sb-1", Status: "active"}, nil
		},
		execFn: func(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
			return canaryapi.ExecResult{ExitCode: 0}, nil
		},
		deleteTemplateFn: func(context.Context, string) error {
			templateDeleted = true
			return nil
		},
	}

	res := templateRunner(client).runTemplate(context.Background(), "run-1")
	if res.Err != nil {
		t.Fatalf("failed step = %q err = %v", res.FailedStep, res.Err)
	}
	if polls != 3 {
		t.Fatalf("expected building, reset, then ready polls, got %d", polls)
	}
	if !sandboxCreated {
		t.Fatal("a reset build poll must not stop the run from launching the template")
	}
	if !templateDeleted {
		t.Fatal("template must be deleted after a passing run")
	}
	got := buf.String()
	if strings.Count(got, `"message":"template build status poll failed with a transient error"`) != 1 {
		t.Fatalf("expected one transient poll log entry: %q", got)
	}
	for _, want := range []string{`"level":"warn"`, `"template_id":"tpl-1"`, `"build_id":"build-1"`, `"attempt":1`, "connection reset by peer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transient poll log entry missing %s: %q", want, got)
		}
	}
}

func TestWaitForBuildStopsAfterRetryBudgetOnRepeatedResets(t *testing.T) {
	t.Parallel()

	polls := 0
	client := &fakeClient{
		getTemplateBuildFn: func(_ context.Context, templateID, buildID string) (canaryapi.TemplateBuild, error) {
			polls++
			return canaryapi.TemplateBuild{}, connectionReset("Get", buildPath(templateID, buildID))
		},
	}

	_, err := templateOps(client).WaitForBuild(context.Background(), "tpl-1", "build-1", WaitForBuildOptions{PollInterval: time.Millisecond})
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("expected the last reset to be returned, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "fetching build status: ") {
		t.Fatalf("unexpected error %v", err)
	}
	if polls != templatePollAttempts {
		t.Fatalf("expected %d attempts, got %d", templatePollAttempts, polls)
	}
}

func TestWaitForBuildStopsRetryingWhenCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polls := 0
	client := &fakeClient{
		getTemplateBuildFn: func(_ context.Context, templateID, buildID string) (canaryapi.TemplateBuild, error) {
			polls++
			// Cancel while the poll sleeps before its first 250ms retry.
			time.AfterFunc(20*time.Millisecond, cancel)
			return canaryapi.TemplateBuild{}, connectionReset("Get", buildPath(templateID, buildID))
		},
	}

	start := time.Now()
	_, err := templateOps(client).WaitForBuild(ctx, "tpl-1", "build-1", WaitForBuildOptions{PollInterval: time.Millisecond})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if polls != 1 {
		t.Fatalf("no retry may run after cancellation, got %d polls", polls)
	}
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("cancellation must interrupt the retry delay, waited %s", elapsed)
	}
}

func TestRunTemplateBuildPollRetriesStopAtRunDeadline(t *testing.T) {
	polls := 0
	templateDeleted := false
	client := &fakeClient{
		createTemplateFn: createTemplateOK,
		getTemplateBuildFn: func(_ context.Context, templateID, buildID string) (canaryapi.TemplateBuild, error) {
			polls++
			if polls == 1 {
				return canaryapi.TemplateBuild{ID: buildID, TemplateID: templateID, Status: "building"}, nil
			}
			return canaryapi.TemplateBuild{}, connectionReset("Get", buildPath(templateID, buildID))
		},
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			t.Fatal("no sandbox should be created when the build wait times out")
			return canaryapi.Sandbox{}, nil
		},
		deleteTemplateFn: func(ctx context.Context, _ string) error {
			if err := ctx.Err(); err != nil {
				t.Fatalf("template delete must not inherit the expired run context: %v", err)
			}
			templateDeleted = true
			return nil
		},
	}
	r := templateRunner(client)
	r.Config.RunTimeout = 50 * time.Millisecond

	start := time.Now()
	err := r.Run(context.Background())
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the run deadline to end the build wait, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "waiting for template build: fetching build status: ") {
		t.Fatalf("unexpected error %v", err)
	}
	// The second poll's reset starts a 250ms retry delay; the 50ms run deadline
	// must cut it short instead of extending the build wait.
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("build wait outlived the run deadline: %s", elapsed)
	}
	if polls != 2 {
		t.Fatalf("expected one building poll and one reset before the deadline, got %d", polls)
	}
	if !templateDeleted {
		t.Fatal("template must be deleted after the run deadline")
	}
}

func TestRunTemplateTerminalBuildStatusIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		status     string
		buildError string
		wantErr    string
	}{
		{status: "failed", buildError: "step_failed: exit status 1", wantErr: "waiting for template build: build failed: step_failed: exit status 1"},
		{status: "cancelled", buildError: "template deleted", wantErr: "waiting for template build: build cancelled: template deleted"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			polls := 0
			templateDeleted := false
			client := &fakeClient{
				createTemplateFn: createTemplateOK,
				getTemplateBuildFn: func(context.Context, string, string) (canaryapi.TemplateBuild, error) {
					polls++
					return canaryapi.TemplateBuild{Status: tc.status, ErrorMessage: tc.buildError}, nil
				},
				createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
					t.Fatalf("no sandbox should be created after a %s build", tc.status)
					return canaryapi.Sandbox{}, nil
				},
				deleteTemplateFn: func(context.Context, string) error {
					templateDeleted = true
					return nil
				},
			}

			res := templateRunner(client).runTemplate(context.Background(), "run-1")
			if res.FailedStep != "template_build_wait" || res.Err == nil || res.Err.Error() != tc.wantErr {
				t.Fatalf("failed step = %q err = %v", res.FailedStep, res.Err)
			}
			if res.BuildError != tc.buildError {
				t.Fatalf("build error = %q", res.BuildError)
			}
			if polls != 1 {
				t.Fatalf("a %s build is terminal and must not be polled again, got %d polls", tc.status, polls)
			}
			if !templateDeleted {
				t.Fatal("template must be deleted when retention is off")
			}
		})
	}
}

func TestWaitForBuildDoesNotRetryPermanentHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "bad request", err: &canaryapi.HTTPStatusError{
			Method:     http.MethodGet,
			Path:       buildPath("tpl-1", "build-1"),
			StatusCode: http.StatusBadRequest,
			Body:       `{"error":{"message":"invalid build id"}}`,
		}},
		{name: "not found", err: fmt.Errorf("GET %s: %w", buildPath("tpl-1", "build-1"), canaryapi.ErrNotFound)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			polls := 0
			client := &fakeClient{
				getTemplateBuildFn: func(context.Context, string, string) (canaryapi.TemplateBuild, error) {
					polls++
					return canaryapi.TemplateBuild{}, tc.err
				},
			}

			_, err := templateOps(client).WaitForBuild(context.Background(), "tpl-1", "build-1", WaitForBuildOptions{PollInterval: time.Millisecond})
			if !errors.Is(err, tc.err) {
				t.Fatalf("expected the permanent error, got %v", err)
			}
			if !strings.HasPrefix(err.Error(), "fetching build status: ") {
				t.Fatalf("unexpected error %v", err)
			}
			if polls != 1 {
				t.Fatalf("permanent errors must not be retried, got %d polls", polls)
			}
		})
	}
}

func TestRunTemplateDeletesTemplateAfterBuildPollRetriesExhausted(t *testing.T) {
	t.Parallel()

	polls := 0
	var deleted []string
	client := &fakeClient{
		createTemplateFn: createTemplateOK,
		getTemplateBuildFn: func(_ context.Context, templateID, buildID string) (canaryapi.TemplateBuild, error) {
			polls++
			return canaryapi.TemplateBuild{}, connectionReset("Get", buildPath(templateID, buildID))
		},
		createSandboxFn: func(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
			t.Fatal("no sandbox should be created after the build poll fails")
			return canaryapi.Sandbox{}, nil
		},
		deleteTemplateFn: func(_ context.Context, id string) error {
			deleted = append(deleted, id)
			return nil
		},
	}
	r := templateRunner(client)
	r.Config.RetainFailedSandbox = false

	res := r.runTemplate(context.Background(), "run-1")
	if res.FailedStep != "template_build_wait" {
		t.Fatalf("failed step = %q err = %v", res.FailedStep, res.Err)
	}
	if !errors.Is(res.Err, syscall.ECONNRESET) || !strings.HasPrefix(res.Err.Error(), "waiting for template build: fetching build status: ") {
		t.Fatalf("expected the original poll error after retries, got %v", res.Err)
	}
	if res.BuildError != "" || res.TemplateID != "tpl-1" || res.BuildID != "build-1" {
		t.Fatalf("unexpected result context %+v", res)
	}
	if polls != templatePollAttempts {
		t.Fatalf("expected %d attempts, got %d", templatePollAttempts, polls)
	}
	if len(deleted) != 1 || deleted[0] != "tpl-1" {
		t.Fatalf("expected one delete of tpl-1, got %v", deleted)
	}
}

func TestRunTemplateDoesNotRetryTemplateCreate(t *testing.T) {
	creates := 0
	client := &fakeClient{
		createTemplateFn: func(context.Context, canaryapi.CreateTemplateRequest) (canaryapi.Template, error) {
			creates++
			return canaryapi.Template{}, connectionReset("Post", "/templates")
		},
		getTemplateBuildFn: func(context.Context, string, string) (canaryapi.TemplateBuild, error) {
			t.Fatal("no build poll should follow a failed create")
			return canaryapi.TemplateBuild{}, nil
		},
		deleteTemplateFn: func(context.Context, string) error {
			t.Fatal("nothing to delete when create failed")
			return nil
		},
	}

	res := templateRunner(client).runTemplate(context.Background(), "run-1")
	if res.FailedStep != "template_create" || !errors.Is(res.Err, syscall.ECONNRESET) {
		t.Fatalf("failed step = %q err = %v", res.FailedStep, res.Err)
	}
	if creates != 1 {
		t.Fatalf("template create must not be retried, got %d attempts", creates)
	}
}
