package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

const buildTokenPath = "/opt/canary/build-token"

type WaitForBuildOptions struct {
	PollInterval time.Duration
	Telemetry    TelemetryContext
}

// runTemplate builds a template whose only step bakes the run token into the
// image, launches a sandbox from it, and reads the token back. The read-back
// is what proves the build steps ran, not just that the build reported ready.
func (r Runner) runTemplate(ctx context.Context, runID string) (res RunResult) {
	ops := r.operations()
	telemetry := r.telemetry()
	resources := RunResources{RunID: runID, CreatedAt: r.Clock().UTC()}
	defer func() {
		if resources.SandboxID != "" {
			res.Err = r.FinalizeSandbox(context.Background(), resources, res)
		}
		if res.TemplateID == "" {
			return
		}
		if res.Err != nil && r.Config.RetainFailedSandbox {
			log.Info().Str("template_id", res.TemplateID).Str("failed_step", res.FailedStep).Msg("template retained for debugging")
			return
		}
		logStep("template_delete")
		if err := ops.DeleteTemplateBestEffort(context.Background(), res.TemplateID, DeleteSandboxOptions{Timeout: r.Config.DeleteTimeout, Telemetry: telemetry}); err != nil {
			log.Warn().Err(err).Str("template_id", res.TemplateID).Msg("template delete failed")
			if res.Err == nil {
				res.Err = err
			}
		}
	}()

	token := "build-" + uuid.NewString()
	buildStart := r.Clock()
	logStep("template_create")
	tpl, err := ops.CreateTemplate(ctx, canaryapi.CreateTemplateRequest{
		Name: templateName(r.Config.Target, runID),
		BuildSpec: canaryapi.BuildSpec{
			From:  r.Config.TemplateBaseImage,
			Steps: []canaryapi.BuildStep{{Run: fmt.Sprintf("sh -c 'mkdir -p /opt/canary && printf %%s %s > %s'", token, buildTokenPath)}},
		},
	}, telemetry)
	if err != nil {
		ops.RecordStep(ctx, telemetry, "template_build_total", result(err), r.Clock().Sub(buildStart))
		res.Err = err
		res.FailedStep = "template_create"
		return res
	}
	res.TemplateID = tpl.ID
	res.BuildID = tpl.BuildID

	logStep("template_build_wait")
	build, err := ops.WaitForBuild(ctx, tpl.ID, tpl.BuildID, WaitForBuildOptions{PollInterval: r.Config.PollInterval, Telemetry: telemetry})
	res.BuildError = build.ErrorMessage
	ops.RecordStep(ctx, telemetry, "template_build_total", result(err), r.Clock().Sub(buildStart))
	if err != nil {
		res.Err = fmt.Errorf("waiting for template build: %w", err)
		res.FailedStep = "template_build_wait"
		return res
	}

	createStart := r.Clock()
	logStep("create_request")
	req := r.canaryCreateSandboxRequest(resources)
	req.FromTemplate = tpl.Name
	sb, err := ops.CreateSandbox(ctx, CreateSandboxOptions{Request: req, Telemetry: telemetry})
	if err != nil {
		ops.RecordStep(ctx, telemetry, "create_total", result(err), r.Clock().Sub(createStart))
		res.Err = err
		res.FailedStep = "create_request"
		return res
	}
	resources.SandboxID = sb.ID
	res.SandboxID = sb.ID

	logStep("create_wait_active")
	if err := ops.WaitForStatusTimed(ctx, sb.ID, WaitForStatusOptions{
		Want:         "active",
		Step:         "create_wait_active",
		PollInterval: r.Config.PollInterval,
		Timeout:      r.Config.CommandTimeout,
		Telemetry:    telemetry,
	}); err != nil {
		ops.RecordStep(ctx, telemetry, "create_total", result(err), r.Clock().Sub(createStart))
		res.Err = fmt.Errorf("waiting for sandbox to become active: %w", err)
		res.FailedStep = "create_wait_active"
		return res
	}
	ops.RecordStep(ctx, telemetry, "create_total", "success", r.Clock().Sub(createStart))

	logStep("verify_build_artifact")
	if _, err := ops.ExecStep(ctx, sb.ID, sb.AccessToken, ExecStepOptions{
		Step:      "verify_build_artifact",
		Command:   fmt.Sprintf("test \"$(cat %s)\" = %s", buildTokenPath, token),
		Timeout:   r.Config.CommandTimeout,
		Telemetry: telemetry,
	}); err != nil {
		res.Err = fmt.Errorf("verifying build artifact: %w", err)
		res.FailedStep = "verify_build_artifact"
		return res
	}
	return res
}

func (o Operations) CreateTemplate(ctx context.Context, req canaryapi.CreateTemplateRequest, telemetry TelemetryContext) (canaryapi.Template, error) {
	start := o.now()
	tpl, err := o.Client.CreateTemplate(ctx, req)
	if err == nil && tpl.BuildID == "" {
		err = errors.New("create response has no build_id")
	}
	o.recordStep(ctx, telemetry, "template_create", result(err), o.now().Sub(start))
	if err != nil {
		return canaryapi.Template{}, fmt.Errorf("creating template: %w", err)
	}
	return tpl, nil
}

// WaitForBuild polls the build row rather than the log stream: the stream's
// terminal event lands before the row flips, and create-from-template 409s
// until the row says ready.
func (o Operations) WaitForBuild(ctx context.Context, templateID, buildID string, opts WaitForBuildOptions) (canaryapi.TemplateBuild, error) {
	start := o.now()
	build, err := o.waitForBuild(ctx, templateID, buildID, opts)
	o.recordStep(ctx, opts.Telemetry, "template_build_wait", result(err), o.now().Sub(start))
	return build, err
}

func (o Operations) waitForBuild(ctx context.Context, templateID, buildID string, opts WaitForBuildOptions) (canaryapi.TemplateBuild, error) {
	interval := opts.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		build, err := o.Client.GetTemplateBuild(ctx, templateID, buildID)
		if err != nil {
			return build, fmt.Errorf("fetching build status: %w", err)
		}
		switch build.Status {
		case "ready":
			return build, nil
		case "failed", "cancelled":
			return build, fmt.Errorf("build %s: %s", build.Status, build.ErrorMessage)
		}
		select {
		case <-ctx.Done():
			return build, fmt.Errorf("waiting for build (last=%s): %w", build.Status, ctx.Err())
		case <-time.After(interval):
		}
	}
}

func (o Operations) DeleteTemplateBestEffort(ctx context.Context, templateID string, opts DeleteSandboxOptions) error {
	start := o.now()
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	err := o.Client.DeleteTemplate(ctx, templateID)
	if errors.Is(err, canaryapi.ErrNotFound) {
		err = nil
	}
	o.RecordCleanup(ctx, opts.Telemetry, result(err))
	o.recordStep(ctx, opts.Telemetry, "template_delete", result(err), o.now().Sub(start))
	if err != nil {
		return fmt.Errorf("deleting template: %w", err)
	}
	return nil
}

func templateName(target, runID string) string {
	return strings.ToLower(sandboxmetadata.TemplateNamePrefix + target + "-" + runID)
}
