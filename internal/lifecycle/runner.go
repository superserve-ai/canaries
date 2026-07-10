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
	Locker  lock.Locker
	Metrics metrics.Provider
	Clock   func() time.Time
	HTTP    *http.Client
}

type Client interface {
	CreateSandbox(context.Context, canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error)
	GetSandbox(context.Context, string) (canaryapi.Sandbox, error)
	PauseSandbox(context.Context, string) error
	ResumeSandbox(context.Context, string) (canaryapi.ResumeResponse, error)
	DeleteSandbox(context.Context, string) error
	Exec(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error)
	PreviewURL(string, int) string
}

func (r Runner) Run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.Config.RunTimeout)
	defer cancel()

	lockResult, err := r.Locker.Acquire(ctx, r.Config.Target, r.Config.LockTTL)
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	if !lockResult.Acquired {
		r.Metrics.RecordOverlapSkip(ctx, r.Config.Environment, r.Config.Region, r.Config.Target)
		log.Info().Str("target", r.Config.Target).Msg("skipping overlapping invocation")
		return nil
	}
	defer func() {
		if err := r.Locker.Release(context.Background(), r.Config.Target, lockResult.LeaseToken); err != nil {
			log.Error().Err(err).Msg("release lock failed")
		}
	}()

	runID := fmt.Sprintf("%s-%s-%d-%s", r.Config.Environment, r.Config.Region, r.Clock().Unix(), uuid.NewString()[:8])
	start := r.Clock()
	result := "failure"
	err = r.runLifecycle(ctx, runID)
	if err == nil {
		result = "success"
	}
	r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", result, r.Clock().Sub(start))
	return err
}

func (r Runner) runLifecycle(ctx context.Context, runID string) (runErr error) {
	expiresAt := r.Clock().Add(r.Config.ResourceTTL).UTC().Format(time.RFC3339)
	metadata := map[string]string{
		"managed_by":    "api-canary",
		"environment":   r.Config.Environment,
		"region":        r.Config.Region,
		"canary_target": r.Config.Target,
		"created_at":    r.Clock().UTC().Format(time.RFC3339),
		"expires_at":    expiresAt,
		"run_id":        runID,
	}
	createStart := r.Clock()
	sb, err := r.Client.CreateSandbox(ctx, canaryapi.CreateSandboxRequest{
		Name:              sandboxName(r.Config.Target, runID),
		TimeoutSeconds:    int(r.Config.RunTimeout.Seconds()),
		AutoDeleteSeconds: int(r.Config.ResourceTTL.Seconds()),
		Metadata:          metadata,
	})
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "create", result(err), r.Clock().Sub(createStart))
	if err != nil {
		return err
	}

	var cleanupErr error
	defer func() {
		if err := r.cleanup(context.Background(), sb.ID); err != nil {
			cleanupErr = err
			if runErr == nil {
				runErr = fmt.Errorf("cleanup: %w", err)
			} else {
				runErr = fmt.Errorf("%w; cleanup: %v", runErr, err)
			}
		}
	}()

	if err := r.waitForStatus(ctx, sb.ID, "active"); err != nil {
		return fmt.Errorf("wait active: %w", err)
	}

	diskToken := "disk-" + uuid.NewString()
	memoryToken := "mem-" + uuid.NewString()
	backgroundCmd := fmt.Sprintf("sh -lc 'printf %%s %q >/tmp/canary-token && nohup python3 -c \"import time; token=%q; open(\\\"/tmp/canary-mem\\\",\\\"w\\\").write(token); time.sleep(3600)\" >/tmp/canary-bg.log 2>&1 & echo started'", diskToken, memoryToken)
	if _, err := r.execStep(ctx, sb.ID, sb.AccessToken, "prime", backgroundCmd); err != nil {
		return err
	}

	if err := r.pause(ctx, sb.ID); err != nil {
		return err
	}
	resumeResp, err := r.resume(ctx, sb.ID)
	if err != nil {
		return err
	}

	verifyCmd := fmt.Sprintf("sh -lc 'test \"$(cat /tmp/canary-token)\" = %q && pgrep -af %q >/dev/null && echo verified'", diskToken, memoryToken)
	if _, err := r.execStep(ctx, sb.ID, resumeResp.AccessToken, "verify_resume", verifyCmd); err != nil {
		return err
	}

	serveToken := "preview-" + runID
	serveCmd := fmt.Sprintf("sh -lc 'mkdir -p /tmp/canary-preview && printf %%s %q >/tmp/canary-preview/index.html && nohup python3 -m http.server %d --directory /tmp/canary-preview >/tmp/canary-preview.log 2>&1 & sleep 1 && echo preview_started'", serveToken, r.Config.PreviewPort)
	if _, err := r.execStep(ctx, sb.ID, resumeResp.AccessToken, "start_preview", serveCmd); err != nil {
		return err
	}
	if err := r.verifyPreview(ctx, sb.ID, serveToken); err != nil {
		return err
	}

	if cleanupErr != nil {
		return cleanupErr
	}
	return nil
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
	return res, err
}

func (r Runner) waitForStatus(ctx context.Context, sandboxID, want string) error {
	deadline := r.Clock().Add(r.Config.CommandTimeout)
	for {
		sb, err := r.Client.GetSandbox(ctx, sandboxID)
		if err != nil {
			return err
		}
		if sb.Status == want {
			return nil
		}
		if sb.Status == "failed" || sb.Status == "deleted" {
			return fmt.Errorf("terminal state %q", sb.Status)
		}
		if r.Clock().After(deadline) {
			return fmt.Errorf("timeout waiting for %s, last=%s", want, sb.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
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
	return err
}

func (r Runner) resume(ctx context.Context, sandboxID string) (canaryapi.ResumeResponse, error) {
	start := r.Clock()
	resp, err := r.Client.ResumeSandbox(ctx, sandboxID)
	if err == nil && resp.Status != "active" {
		err = fmt.Errorf("unexpected resume status %q", resp.Status)
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "resume", result(err), r.Clock().Sub(start))
	return resp, err
}

func (r Runner) verifyPreview(ctx context.Context, sandboxID, expected string) error {
	start := r.Clock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Client.PreviewURL(sandboxID, r.Config.PreviewPort), nil)
	if err != nil {
		return err
	}
	httpClient := r.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "preview", "failure", r.Clock().Sub(start))
		return err
	}
	defer resp.Body.Close()
	buf := make([]byte, len(expected)+8)
	n, _ := resp.Body.Read(buf)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(buf[:n]), expected) {
		err = errors.New("preview response mismatch")
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "preview", result(err), r.Clock().Sub(start))
	return err
}

func (r Runner) cleanup(ctx context.Context, sandboxID string) error {
	start := r.Clock()
	ctx, cancel := context.WithTimeout(ctx, r.Config.DeleteTimeout)
	defer cancel()
	err := r.Client.DeleteSandbox(ctx, sandboxID)
	if errors.Is(err, canaryapi.ErrNotFound) {
		err = nil
	}
	r.Metrics.RecordCleanup(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, result(err))
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "delete", result(err), r.Clock().Sub(start))
	return err
}

func sandboxName(target, runID string) string {
	return fmt.Sprintf("api-canary-%s-%s", target, runID)
}

func result(err error) string {
	if err != nil {
		return "failure"
	}
	return "success"
}
