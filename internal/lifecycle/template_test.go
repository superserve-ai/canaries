package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

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
