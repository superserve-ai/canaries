package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
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
}

type StepError struct {
	Step string
	Err  error
}

func (e StepError) Error() string {
	if e.Err == nil {
		return e.Step
	}
	return e.Err.Error()
}

func (e StepError) Unwrap() error { return e.Err }

type Client interface {
	CreateSandbox(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error)
	GetSandbox(context.Context, string) (canaryapi.Sandbox, error)
	PauseSandbox(context.Context, string) error
	ResumeSandbox(context.Context, string) (canaryapi.ResumeResponse, error)
	DeleteSandbox(context.Context, string) error
	UpdateSandbox(context.Context, string, canaryapi.UpdateSandboxRequest) error
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
	runResult := r.runLifecycle(ctx, runID)
	err = runResult.Err
	if err == nil {
		result = "success"
	}
	r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", result, r.Clock().Sub(start))
	return err
}

func (r Runner) runLifecycle(ctx context.Context, runID string) (res RunResult) {
	resources := RunResources{RunID: runID, CreatedAt: r.Clock().UTC()}
	defer func() {
		if resources.SandboxID == "" {
			return
		}
		res.Err = r.finalizeSandbox(context.Background(), resources, res)
	}()

	expiresAt := resources.CreatedAt.Add(r.retentionTTL()).UTC().Format(time.RFC3339)
	metadata := map[string]string{
		"managed_by":    "api-canary",
		"environment":   r.Config.Environment,
		"region":        r.Config.Region,
		"canary_target": r.Config.Target,
		"created_at":    resources.CreatedAt.Format(time.RFC3339),
		"expires_at":    expiresAt,
		"run_id":        runID,
	}
	createStart := r.Clock()
	sb, err := r.Client.CreateSandbox(ctx, canaryapi.CreateSandboxRequest{
		Name:              sandboxName(r.Config.Target, runID),
		FromTemplate:      r.Config.SandboxTemplate,
		TimeoutSeconds:    int(r.Config.RunTimeout.Seconds()),
		AutoDeleteSeconds: int(r.retentionTTL().Seconds()),
		Metadata:          metadata,
	})
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "create", result(err), r.Clock().Sub(createStart))
	if err != nil {
		res.Err = fmt.Errorf("creating sandbox: %w", err)
		res.FailedStep = "create_sandbox"
		return res
	}
	resources.SandboxID = sb.ID

	if err := r.waitForStatus(ctx, sb.ID, "active"); err != nil {
		res.Err = fmt.Errorf("waiting for sandbox to become active: %w", err)
		res.FailedStep = "wait_ready"
		return res
	}

	diskToken := "disk-" + uuid.NewString()
	memoryToken := "mem-" + uuid.NewString()
	backgroundCmd := fmt.Sprintf("sh -lc 'printf %%s %q >/tmp/canary-token && nohup python3 -c \"import time; token=%q; open(\\\"/tmp/canary-mem\\\",\\\"w\\\").write(token); time.sleep(3600)\" >/tmp/canary-bg.log 2>&1 & echo started'", diskToken, memoryToken)
	if _, err := r.execStep(ctx, sb.ID, sb.AccessToken, "initial_command", backgroundCmd); err != nil {
		res.Err = fmt.Errorf("priming sandbox: %w", err)
		res.FailedStep = "initial_command"
		return res
	}

	if err := r.pause(ctx, sb.ID); err != nil {
		res.Err = fmt.Errorf("pausing sandbox: %w", err)
		res.FailedStep = "pause"
		return res
	}
	if err := r.waitForStatus(ctx, sb.ID, "paused"); err != nil {
		res.Err = fmt.Errorf("waiting for sandbox to pause: %w", err)
		res.FailedStep = "wait_paused"
		return res
	}

	resumeResp, err := r.resume(ctx, sb.ID)
	if err != nil {
		res.Err = fmt.Errorf("resuming sandbox: %w", err)
		res.FailedStep = "resume"
		return res
	}
	if err := r.waitForStatus(ctx, sb.ID, "active"); err != nil {
		res.Err = fmt.Errorf("waiting for sandbox to resume: %w", err)
		res.FailedStep = "wait_resumed"
		return res
	}

	if err := r.uploadVerificationUtilities(ctx, sb.ID, resumeResp.AccessToken); err != nil {
		res.Err = err
		res.FailedStep = "prepare_verification_utilities"
		return res
	}

	verifyDiskCmd := fmt.Sprintf("sh -lc 'CANARY_DISK_TOKEN=%q /tmp/verification-utilities/verify_disk.sh'", diskToken)
	if _, err := r.execStep(ctx, sb.ID, resumeResp.AccessToken, "verify_disk", verifyDiskCmd); err != nil {
		res.Err = fmt.Errorf("verifying disk state: %w", err)
		res.FailedStep = "verify_disk"
		return res
	}

	verifyMemoryCmd := fmt.Sprintf("sh -lc 'CANARY_MEMORY_TOKEN=%q /tmp/verification-utilities/verify_memory.py'", memoryToken)
	if _, err := r.execStep(ctx, sb.ID, resumeResp.AccessToken, "verify_memory", verifyMemoryCmd); err != nil {
		res.Err = fmt.Errorf("verifying memory state: %w", err)
		res.FailedStep = "verify_memory"
		return res
	}

	serveToken := "preview-" + runID
	serveCmd := fmt.Sprintf("sh -lc 'mkdir -p /tmp/canary-preview && printf %%s %q >/tmp/canary-preview/index.html && nohup python3 -m http.server %d --directory /tmp/canary-preview >/tmp/canary-preview.log 2>&1 & sleep 1 && echo preview_started'", serveToken, r.Config.PreviewPort)
	if _, err := r.execStep(ctx, sb.ID, resumeResp.AccessToken, "start_http_server", serveCmd); err != nil {
		res.Err = fmt.Errorf("starting preview server: %w", err)
		res.FailedStep = "start_http_server"
		return res
	}
	if err := r.verifyPreview(ctx, sb.ID, serveToken); err != nil {
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

func (r Runner) execStep(ctx context.Context, sandboxID, accessToken, step, command string) (canaryapi.ExecResult, error) {
	start := r.Clock()
	res, err := r.Client.Exec(ctx, sandboxID, accessToken, canaryapi.ExecRequest{
		Command:  command,
		TimeoutS: int(r.Config.CommandTimeout.Seconds()),
	})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("command failed: exit=%d stderr=%s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", step, result(err), r.Clock().Sub(start))
	if err != nil {
		err = fmt.Errorf("running %s step: %w", step, err)
	}
	return res, err
}

func (r Runner) waitForStatus(ctx context.Context, sandboxID, want string) error {
	deadline := r.Clock().Add(r.Config.CommandTimeout)
	for {
		sb, err := r.Client.GetSandbox(ctx, sandboxID)
		if err != nil {
			return fmt.Errorf("fetching sandbox status: %w", err)
		}
		if sb.Status == want {
			return nil
		}
		if sb.Status == "failed" || sb.Status == "deleted" {
			return fmt.Errorf("sandbox entered terminal state %q", sb.Status)
		}
		if r.Clock().After(deadline) {
			return fmt.Errorf("timed out waiting for sandbox to become %s, last=%s", want, sb.Status)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for sandbox status %q: %w", want, ctx.Err())
		case <-time.After(r.Config.PollInterval):
		}
	}
}

func (r Runner) pause(ctx context.Context, sandboxID string) error {
	start := r.Clock()
	err := r.Client.PauseSandbox(ctx, sandboxID)
	if err == nil {
		err = r.waitForStatus(ctx, sandboxID, "paused")
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "pause", result(err), r.Clock().Sub(start))
	if err != nil {
		err = fmt.Errorf("pausing sandbox: %w", err)
	}
	return err
}

func (r Runner) resume(ctx context.Context, sandboxID string) (canaryapi.ResumeResponse, error) {
	start := r.Clock()
	resp, err := r.Client.ResumeSandbox(ctx, sandboxID)
	if err == nil && resp.Status != "active" {
		err = fmt.Errorf("unexpected resume status %q", resp.Status)
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "resume", result(err), r.Clock().Sub(start))
	if err != nil {
		err = fmt.Errorf("resuming sandbox: %w", err)
	}
	return resp, err
}

func (r Runner) verifyPreview(ctx context.Context, sandboxID, expected string) error {
	start := r.Clock()
	previewURL := r.Client.PreviewURL(sandboxID, r.Config.PreviewPort)
	deadline := r.Clock().Add(r.Config.PreviewTimeout)
	interval := r.Config.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
loop:
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, previewURL, nil)
		if err != nil {
			r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "preview", "failure", r.Clock().Sub(start))
			return StepError{Step: "resolve_preview_url", Err: fmt.Errorf("building preview request for %s: %w", previewURL, err)}
		}
		httpClient := r.HTTP
		if httpClient == nil {
			httpClient = http.DefaultClient
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("reading preview response from %s: %w", previewURL, readErr)
			} else if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), expected) {
				trimmed := strings.TrimSpace(string(body))
				if resp.StatusCode == http.StatusBadGateway && strings.Contains(strings.ToLower(trimmed), "sandbox unreachable") {
					lastErr = fmt.Errorf("preview proxy could not reach the sandbox listener: status=%d body=%q", resp.StatusCode, trimmed)
				} else {
					lastErr = fmt.Errorf("preview response mismatch: status=%d body=%q", resp.StatusCode, trimmed)
				}
			} else {
				r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "preview", result(nil), r.Clock().Sub(start))
				return nil
			}
		}
		if !r.Clock().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
			break loop
		case <-ticker.C:
		}
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "preview", "failure", r.Clock().Sub(start))
	return StepError{Step: "check_preview_url", Err: fmt.Errorf("verifying preview URL %s: %w", previewURL, lastErr)}
}

func (r Runner) retentionTTL() time.Duration {
	if r.Config.RetainFailedSandbox && r.Config.RetainFailedSandboxTTL > 0 {
		if r.Config.RetainFailedSandboxTTL > r.Config.ResourceTTL {
			return r.Config.RetainFailedSandboxTTL
		}
	}
	return r.Config.ResourceTTL
}

func (r Runner) finalizeSandbox(ctx context.Context, resources RunResources, res RunResult) error {
	if res.Err != nil && r.Config.RetainFailedSandbox {
		if err := r.retainSandbox(ctx, resources, res); err != nil {
			log.Warn().
				Err(err).
				Str("sandbox_id", resources.SandboxID).
				Str("run_id", resources.RunID).
				Str("failed_step", res.FailedStep).
				Msg("sandbox retention metadata update failed")
		}
		return res.Err
	}
	if err := r.deleteSandboxBestEffort(ctx, resources.SandboxID); err != nil {
		if res.Err != nil {
			log.Warn().Err(err).Str("sandbox_id", resources.SandboxID).Msg("sandbox delete failed")
			return res.Err
		}
		return err
	}
	return res.Err
}

func (r Runner) retainSandbox(ctx context.Context, resources RunResources, res RunResult) error {
	retainedAt := r.Clock().UTC()
	expiresAt := retainedAt.Add(r.Config.RetainFailedSandboxTTL).UTC()
	metadata := map[string]string{
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
	if err := r.Client.UpdateSandbox(ctx, resources.SandboxID, canaryapi.UpdateSandboxRequest{
		Metadata:          metadata,
		AutoDeleteSeconds: func() *int { v := int(r.Config.RetainFailedSandboxTTL.Seconds()); return &v }(),
	}); err != nil {
		return fmt.Errorf("update retention metadata: %w", err)
	}
	r.Metrics.RecordRetainedSandbox(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, res.FailedStep)
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
	return nil
}

func (r Runner) deleteSandboxBestEffort(ctx context.Context, sandboxID string) error {
	start := r.Clock()
	ctx, cancel := context.WithTimeout(ctx, r.Config.DeleteTimeout)
	defer cancel()
	err := r.Client.DeleteSandbox(ctx, sandboxID)
	if errors.Is(err, canaryapi.ErrNotFound) {
		err = nil
	}
	r.Metrics.RecordCleanup(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, result(err))
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "delete_sandbox", result(err), r.Clock().Sub(start))
	if err != nil {
		return fmt.Errorf("deleting sandbox: %w", err)
	}
	return nil
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
