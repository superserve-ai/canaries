package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
)

type Runner struct {
	Config  config.Config
	Client  Client
	Locker  lock.Lock
	Metrics metrics.Provider
	Clock   func() time.Time
	HTTP    *http.Client
}

type RunResources struct {
	SandboxID string
	RunID     string
	CreatedAt time.Time
}

type RunResult struct {
	Err        error
	FailedStep string
	SandboxID  string
}

type ExecValidationError struct {
	ExitCode int
	Stderr   string
}

type StepError struct {
	Step string
	Err  error
}

func logStep(step string) {
	log.Info().
		Str("step", step).
		Msg("canary step started")
}

func (e StepError) Error() string {
	if e.Err == nil {
		return e.Step
	}
	return e.Err.Error()
}

func (e ExecValidationError) Error() string {
	return fmt.Sprintf("command failed: exit=%d stderr=%s", e.ExitCode, strings.TrimSpace(e.Stderr))
}

func (e StepError) Unwrap() error { return e.Err }

func (r Runner) operations() Operations {
	return Operations{
		Client:  r.Client,
		Metrics: r.Metrics,
		Clock:   r.Clock,
		HTTP:    r.HTTP,
	}
}

func (r Runner) telemetry() TelemetryContext {
	return TelemetryContext{
		Environment: r.Config.Environment,
		Region:      r.Config.Region,
		Target:      r.Config.Target,
		Scenario:    "lifecycle",
	}
}

type Client interface {
	CreateSandbox(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error)
	GetSandbox(context.Context, string) (canaryapi.Sandbox, error)
	PauseSandbox(context.Context, string) error
	ResumeSandbox(context.Context, string) (canaryapi.ResumeResponse, error)
	DeleteSandbox(context.Context, string) error
	UpdateSandbox(context.Context, string, canaryapi.UpdateSandboxRequest) error
	PublishPreviewPort(context.Context, string, canaryapi.PublishPreviewPortRequest) error
	WriteFile(context.Context, string, string, string, []byte) error
	Exec(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error)
	PreviewURL(string, int) string
}

func (r Runner) Run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.Config.RunTimeout)
	defer cancel()

	outcome, lease, err := r.Locker.Acquire(ctx, r.Config.Target, r.Config.LockTTL)
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	if outcome == lock.OutcomeAlreadyRunning {
		r.Metrics.RecordOverlapSkip(ctx, r.Config.Environment, r.Config.Region, r.Config.Target)
		log.Info().Str("target", r.Config.Target).Msg("canary skipped because another run holds the target lock")
		return nil
	}
	defer func() {
		if lease == nil {
			return
		}
		if err := lease.Release(context.Background()); err != nil {
			log.Error().Err(err).Msg("release lock failed")
		}
	}()

	r.Metrics.RecordExecutionDelta(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", 1)
	defer r.Metrics.RecordExecutionDelta(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", -1)

	runID := fmt.Sprintf("%s-%s-%d-%s", r.Config.Environment, r.Config.Region, r.Clock().Unix(), uuid.NewString()[:8])
	start := r.Clock()
	result := "failure"

	log.Info().
		Str("run_id", runID).
		Str("target", r.Config.Target).
		Str("environment", r.Config.Environment).
		Str("region", r.Config.Region).
		Msg("lifecycle canary started")

	runResult := r.runLifecycle(ctx, runID)
	err = runResult.Err
	if err == nil {
		result = "success"
	}
	duration := r.Clock().Sub(start)
	r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", result, duration)

	if err != nil {
		log.Error().
			Err(err).
			Str("run_id", runID).
			Str("sandbox_id", runResult.SandboxID).
			Str("result", result).
			Str("failed_step", runResult.FailedStep).
			Dur("duration", duration).
			Msg("lifecycle canary completed")
		return err
	}

	log.Info().
		Str("run_id", runID).
		Str("result", result).
		Dur("duration", duration).
		Msg("lifecycle canary completed")
	return nil
}

func (r Runner) RunLifecycle(ctx context.Context, runID string) RunResult {
	return r.runLifecycle(ctx, runID)
}

func (r Runner) runLifecycle(ctx context.Context, runID string) (res RunResult) {
	ops := r.operations()
	telemetry := r.telemetry()
	resources := RunResources{RunID: runID, CreatedAt: r.Clock().UTC()}
	defer func() {
		if resources.SandboxID == "" {
			return
		}
		res.Err = r.FinalizeSandbox(context.Background(), resources, res)
	}()

	createStart := r.Clock()
	logStep("create_request")
	sb, err := ops.CreateSandbox(ctx, CreateSandboxOptions{
		Request:   r.canaryCreateSandboxRequest(resources),
		Telemetry: telemetry,
	})
	if err != nil {
		r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "create_total", result(err), r.Clock().Sub(createStart))
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
		r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "create_total", result(err), r.Clock().Sub(createStart))
		res.Err = fmt.Errorf("waiting for sandbox to become active: %w", err)
		res.FailedStep = "create_wait_active"
		return res
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "create_total", "success", r.Clock().Sub(createStart))

	diskToken := "disk-" + uuid.NewString()
	memoryToken := "mem-" + uuid.NewString()
	accessTokenPrefix := sb.AccessToken
	if len(accessTokenPrefix) > 8 {
		accessTokenPrefix = accessTokenPrefix[:8]
	}
	log.Info().
		Str("sandbox_id", sb.ID).
		Str("access_token_prefix", accessTokenPrefix).
		Msg("writing canary token")
	logStep("seed_canary_token")
	if err := ops.WriteSandboxFileWithRetry(ctx, sb.ID, sb.AccessToken, "/tmp/canary-token", []byte(diskToken)); err != nil {
		res.Err = fmt.Errorf("seeding canary token: %w", err)
		res.FailedStep = "seed_canary_token"
		return res
	}

	backgroundCmd := fmt.Sprintf("sh -lc 'nohup python3 -c \"import time; time.sleep(3600)\" %q >/tmp/canary-bg.log 2>&1 & echo started'", memoryToken)
	logStep("initial_command")
	if _, err := ops.ExecStep(ctx, sb.ID, sb.AccessToken, ExecStepOptions{
		Step:      "initial_command",
		Command:   backgroundCmd,
		Timeout:   r.Config.CommandTimeout,
		Telemetry: telemetry,
	}); err != nil {
		res.Err = fmt.Errorf("priming sandbox: %w", err)
		res.FailedStep = "initial_command"
		return res
	}

	pauseStart := r.Clock()
	logStep("pause_request")
	if err := ops.Pause(ctx, sb.ID, telemetry); err != nil {
		r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "pause_total", result(err), r.Clock().Sub(pauseStart))
		res.Err = fmt.Errorf("pausing sandbox: %w", err)
		res.FailedStep = "pause_request"
		return res
	}
	logStep("pause_wait_paused")
	if err := ops.WaitForStatusTimed(ctx, sb.ID, WaitForStatusOptions{
		Want:         "paused",
		Step:         "pause_wait_paused",
		PollInterval: r.Config.PollInterval,
		Timeout:      r.Config.CommandTimeout,
		Telemetry:    telemetry,
	}); err != nil {
		r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "pause_total", result(err), r.Clock().Sub(pauseStart))
		res.Err = fmt.Errorf("waiting for sandbox to pause: %w", err)
		res.FailedStep = "pause_wait_paused"
		return res
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "pause_total", "success", r.Clock().Sub(pauseStart))

	resumeStart := r.Clock()
	logStep("resume_request")
	resumeResp, err := ops.Resume(ctx, sb.ID, telemetry)
	if err != nil {
		r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "resume_total", result(err), r.Clock().Sub(resumeStart))
		res.Err = fmt.Errorf("resuming sandbox: %w", err)
		res.FailedStep = "resume_request"
		return res
	}
	logStep("resume_wait_active")
	if err := ops.WaitForStatusTimed(ctx, sb.ID, WaitForStatusOptions{
		Want:         "active",
		Step:         "resume_wait_active",
		PollInterval: r.Config.PollInterval,
		Timeout:      r.Config.CommandTimeout,
		Telemetry:    telemetry,
	}); err != nil {
		r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "resume_total", result(err), r.Clock().Sub(resumeStart))
		res.Err = fmt.Errorf("waiting for sandbox to resume: %w", err)
		res.FailedStep = "resume_wait_active"
		return res
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "resume_total", "success", r.Clock().Sub(resumeStart))

	logStep("prepare_verification_utilities")
	if err := ops.UploadVerificationUtilities(ctx, VerificationUtilitiesInput{
		SandboxID:   sb.ID,
		AccessToken: resumeResp.AccessToken,
		Step:        "prepare_verification_utilities",
		Telemetry:   telemetry,
	}); err != nil {
		res.Err = err
		res.FailedStep = "prepare_verification_utilities"
		return res
	}

	verifyDiskCmd := fmt.Sprintf("sh -lc 'CANARY_DISK_TOKEN=%q sh /tmp/verification-utilities/verify_disk.sh'", diskToken)
	logStep("verify_disk")
	if _, err := ops.ExecStep(ctx, sb.ID, resumeResp.AccessToken, ExecStepOptions{
		Step:      "verify_disk",
		Command:   verifyDiskCmd,
		Timeout:   r.Config.CommandTimeout,
		Telemetry: telemetry,
	}); err != nil {
		res.Err = fmt.Errorf("verifying disk state: %w", err)
		res.FailedStep = "verify_disk"
		return res
	}

	verifyMemoryCmd := fmt.Sprintf("sh -lc 'CANARY_MEMORY_TOKEN=%q python3 /tmp/verification-utilities/verify_memory.py'", memoryToken)
	logStep("verify_memory")
	if _, err := ops.ExecStep(ctx, sb.ID, resumeResp.AccessToken, ExecStepOptions{
		Step:      "verify_memory",
		Command:   verifyMemoryCmd,
		Timeout:   r.Config.CommandTimeout,
		Telemetry: telemetry,
	}); err != nil {
		res.Err = fmt.Errorf("verifying memory state: %w", err)
		res.FailedStep = "verify_memory"
		return res
	}

	serveToken := "preview-" + runID
	logStep("seed_preview_page")
	if err := ops.WriteSandboxFileWithRetry(ctx, sb.ID, resumeResp.AccessToken, "/tmp/canary-preview/index.html", []byte(serveToken)); err != nil {
		res.Err = fmt.Errorf("seeding preview page: %w", err)
		res.FailedStep = "seed_preview_page"
		return res
	}

	serveCmd := fmt.Sprintf("sh -lc 'mkdir -p /tmp/canary-preview && nohup python3 -m http.server %d --directory /tmp/canary-preview >/tmp/canary-preview.log 2>&1 & sleep 1 && echo preview_started'", r.Config.PreviewPort)
	logStep("start_http_server")
	if _, err := ops.ExecStep(ctx, sb.ID, resumeResp.AccessToken, ExecStepOptions{
		Step:      "start_http_server",
		Command:   serveCmd,
		Timeout:   r.Config.CommandTimeout,
		Telemetry: telemetry,
	}); err != nil {
		res.Err = fmt.Errorf("starting preview server: %w", err)
		res.FailedStep = "start_http_server"
		return res
	}

	logStep("publish_preview_port")
	err = ops.PublishPreviewPort(ctx, sb.ID, PublishPreviewPortOptions{
		Port:      r.Config.PreviewPort,
		Access:    "public",
		Telemetry: telemetry,
	})
	if err != nil {
		res.Err = fmt.Errorf("publishing preview port: %w", err)
		res.FailedStep = "publish_preview_port"
		return res
	}

	logStep("check_preview_url")
	if err := ops.VerifyPreview(ctx, sb.ID, serveToken, VerifyPreviewOptions{
		Port:         r.Config.PreviewPort,
		Timeout:      r.Config.PreviewTimeout,
		PollInterval: r.Config.PollInterval,
		Telemetry:    telemetry,
	}); err != nil {
		res.Err = fmt.Errorf("verifying preview URL: %w", err)
		res.FailedStep = "check_preview_url"
		var stepErr StepError
		if errors.As(err, &stepErr) {
			res.FailedStep = stepErr.Step
		}
		return res
	}

	return res
}

func (r Runner) retentionTTL() time.Duration {
	if r.Config.RetainFailedSandbox && r.Config.RetainFailedSandboxTTL > 0 {
		if r.Config.RetainFailedSandboxTTL > r.Config.ResourceTTL {
			return r.Config.RetainFailedSandboxTTL
		}
	}
	return r.Config.ResourceTTL
}

func (r Runner) FinalizeSandbox(ctx context.Context, resources RunResources, res RunResult) error {
	telemetry := r.telemetry()
	if !(res.Err != nil && r.Config.RetainFailedSandbox) {
		logStep("delete_request")
	}

	outcome, err := r.operations().FinalizeSandbox(ctx, resources, res, FinalizeOptions{
		Delete: DeleteSandboxOptions{
			Timeout:   r.Config.DeleteTimeout,
			Telemetry: telemetry,
		},
		Retain: RetentionOptions{
			Enabled:           r.Config.RetainFailedSandbox,
			Metadata:          r.canaryRetainMetadata(resources, res),
			AutoDeleteSeconds: func() *int { v := int(r.Config.RetainFailedSandboxTTL.Seconds()); return &v }(),
		},
		Telemetry: telemetry,
	})
	if outcome.RetentionError != nil {
		log.Warn().
			Err(outcome.RetentionError).
			Str("sandbox_id", resources.SandboxID).
			Str("run_id", resources.RunID).
			Str("failed_step", res.FailedStep).
			Msg("sandbox retention metadata update failed")
	}
	if outcome.DeleteError != nil && res.Err != nil {
		log.Warn().Err(outcome.DeleteError).Str("sandbox_id", resources.SandboxID).Msg("sandbox delete failed")
	}
	if outcome.Retained {
		retainedAt := r.Clock().UTC()
		expiresAt := retainedAt.Add(r.Config.RetainFailedSandboxTTL).UTC()
		log.Info().
			Str("event", "canary_sandbox_retained").
			Str("target", r.Config.Target).
			Str("environment", r.Config.Environment).
			Str("region", r.Config.Region).
			Str("run_id", resources.RunID).
			Str("sandbox_id", resources.SandboxID).
			Str("failed_step", res.FailedStep).
			Str("retained_at", retainedAt.Format(time.RFC3339)).
			Str("expires_at", expiresAt.Format(time.RFC3339)).
			Msg("sandbox retained for debugging; inspect sandbox_id=" + resources.SandboxID + "; janitor expiration=" + expiresAt.Format(time.RFC3339))
	}
	return err
}

func (r Runner) lifecycleMetadata(resources RunResources) map[string]string {
	expiresAt := resources.CreatedAt.Add(r.retentionTTL()).UTC().Format(time.RFC3339)
	return map[string]string{
		"managed_by":    "api-canary",
		"environment":   r.Config.Environment,
		"region":        r.Config.Region,
		"canary_target": r.Config.Target,
		"created_at":    resources.CreatedAt.Format(time.RFC3339),
		"expires_at":    expiresAt,
		"run_id":        resources.RunID,
	}
}

func (r Runner) canaryCreateSandboxRequest(resources RunResources) canaryapi.CreateSandboxRequest {
	return canaryapi.CreateSandboxRequest{
		Name:              sandboxName(r.Config.Target, resources.RunID),
		FromTemplate:      r.Config.SandboxTemplate,
		TimeoutSeconds:    int(r.Config.RunTimeout.Seconds()),
		AutoDeleteSeconds: int(r.retentionTTL().Seconds()),
		Metadata:          r.lifecycleMetadata(resources),
	}
}

func (r Runner) canaryRetainMetadata(resources RunResources, res RunResult) map[string]string {
	retainedAt := r.Clock().UTC()
	expiresAt := retainedAt.Add(r.Config.RetainFailedSandboxTTL).UTC()
	return map[string]string{
		"managed_by":         "api-canary",
		"canary_target":      r.Config.Target,
		"environment":        r.Config.Environment,
		"region":             r.Config.Region,
		"run_id":             resources.RunID,
		"created_at":         resources.CreatedAt.Format(time.RFC3339),
		"retained_for_debug": "true",
		"failed_step":        res.FailedStep,
		"retained_at":        retainedAt.Format(time.RFC3339),
		"expires_at":         expiresAt.Format(time.RFC3339),
	}
}

func (r Runner) CreateSandbox(ctx context.Context, runID string) (RunResources, canaryapi.Sandbox, error) {
	resources := RunResources{RunID: runID, CreatedAt: r.Clock().UTC()}
	createStart := r.Clock()
	sb, err := r.operations().CreateSandbox(ctx, CreateSandboxOptions{
		Request:   r.canaryCreateSandboxRequest(resources),
		Telemetry: r.telemetry(),
	})
	if err != nil {
		r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "create_total", result(err), r.Clock().Sub(createStart))
		return resources, canaryapi.Sandbox{}, err
	}
	resources.SandboxID = sb.ID
	if err := r.operations().WaitForStatusTimed(ctx, sb.ID, WaitForStatusOptions{
		Want:         "active",
		Step:         "create_wait_active",
		PollInterval: r.Config.PollInterval,
		Timeout:      r.Config.CommandTimeout,
		Telemetry:    r.telemetry(),
	}); err != nil {
		r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "create_total", result(err), r.Clock().Sub(createStart))
		return resources, canaryapi.Sandbox{}, StepError{Step: "create_wait_active", Err: fmt.Errorf("waiting for sandbox to become active: %w", err)}
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "create_total", "success", r.Clock().Sub(createStart))
	return resources, sb, nil
}

func (r Runner) WaitForStatus(ctx context.Context, sandboxID, want string) error {
	return r.operations().WaitForStatus(ctx, sandboxID, WaitForStatusOptions{
		Want:         want,
		PollInterval: r.Config.PollInterval,
		Timeout:      r.Config.CommandTimeout,
	})
}

func (r Runner) WriteSandboxFileWithRetry(ctx context.Context, sandboxID, accessToken, path string, content []byte) error {
	return r.operations().WriteSandboxFileWithRetry(ctx, sandboxID, accessToken, path, content)
}

func (r Runner) UploadVerificationUtilities(ctx context.Context, sandboxID, accessToken string) error {
	return r.operations().UploadVerificationUtilities(ctx, VerificationUtilitiesInput{
		SandboxID:   sandboxID,
		AccessToken: accessToken,
		Step:        "prepare_verification_utilities",
		Telemetry:   r.telemetry(),
	})
}

func (r Runner) Exec(ctx context.Context, sandboxID, accessToken string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
	return r.operations().Exec(ctx, sandboxID, accessToken, req)
}

func (r Runner) ExecStep(ctx context.Context, sandboxID, accessToken, step, command string) (canaryapi.ExecResult, error) {
	return r.operations().ExecStep(ctx, sandboxID, accessToken, ExecStepOptions{
		Step:      step,
		Command:   command,
		Timeout:   r.Config.CommandTimeout,
		Telemetry: r.telemetry(),
	})
}

func (r Runner) DeleteSandboxBestEffort(ctx context.Context, sandboxID string) error {
	return r.operations().DeleteSandboxBestEffort(ctx, sandboxID, DeleteSandboxOptions{
		Timeout:   r.Config.DeleteTimeout,
		Telemetry: r.telemetry(),
	})
}

func (r Runner) PublishPreviewPort(ctx context.Context, sandboxID string) error {
	return r.operations().PublishPreviewPort(ctx, sandboxID, PublishPreviewPortOptions{
		Port:      r.Config.PreviewPort,
		Access:    "public",
		Telemetry: r.telemetry(),
	})
}

func (r Runner) VerifyPreview(ctx context.Context, sandboxID, expected string) error {
	return r.operations().VerifyPreview(ctx, sandboxID, expected, VerifyPreviewOptions{
		Port:         r.Config.PreviewPort,
		Timeout:      r.Config.PreviewTimeout,
		PollInterval: r.Config.PollInterval,
		Telemetry:    r.telemetry(),
	})
}

func sandboxName(target, runID string) string {
	const (
		prefix = "api-canary-"
		limit  = 64
	)
	name := fmt.Sprintf("%s%s-%s", prefix, target, runID)
	if len(name) <= limit {
		return name
	}
	maxRunIDLen := limit - len(prefix) - len(target) - 1
	if maxRunIDLen < 8 {
		maxTargetLen := limit - len(prefix) - 1 - 8
		if maxTargetLen < 1 {
			maxTargetLen = 1
		}
		if len(target) > maxTargetLen {
			target = target[:maxTargetLen]
		}
		maxRunIDLen = limit - len(prefix) - len(target) - 1
		if maxRunIDLen < 8 {
			maxRunIDLen = 8
		}
	}
	if len(runID) > maxRunIDLen {
		runID = runID[:maxRunIDLen]
	}
	return fmt.Sprintf("%s%s-%s", prefix, target, runID)
}

func result(err error) string {
	if err != nil {
		return "failure"
	}
	return "success"
}
