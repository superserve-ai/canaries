package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/metrics"
)

type TelemetryContext struct {
	Environment string
	Region      string
	Target      string
	Scenario    string
}

type Operations struct {
	Client  Client
	Metrics metrics.Provider
	Clock   func() time.Time
	HTTP    *http.Client
}

type CreateSandboxOptions struct {
	Request   canaryapi.CreateSandboxRequest
	Telemetry TelemetryContext
}

type WaitForStatusOptions struct {
	Want         string
	Step         string
	PollInterval time.Duration
	Timeout      time.Duration
	Telemetry    TelemetryContext
}

type PublishPreviewPortOptions struct {
	Port      int
	Access    string
	Telemetry TelemetryContext
}

type ExecStepOptions struct {
	Step      string
	Command   string
	Timeout   time.Duration
	Telemetry TelemetryContext
}

type VerifyPreviewOptions struct {
	Port         int
	Timeout      time.Duration
	PollInterval time.Duration
	Telemetry    TelemetryContext
}

type DeleteSandboxOptions struct {
	Timeout   time.Duration
	Telemetry TelemetryContext
}

type RetentionOptions struct {
	Enabled           bool
	Metadata          map[string]string
	AutoDeleteSeconds *int
}

type FinalizeOptions struct {
	Delete    DeleteSandboxOptions
	Retain    RetentionOptions
	Telemetry TelemetryContext
}

type FinalizeResult struct {
	Retained       bool
	RetentionError error
	DeleteError    error
}

func (o Operations) now() time.Time {
	if o.Clock != nil {
		return o.Clock()
	}
	return time.Now()
}

func (o Operations) metricsProvider() metrics.Provider {
	if o.Metrics != nil {
		return o.Metrics
	}
	return metrics.NoopProvider{}
}

func (o Operations) httpClient() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return http.DefaultClient
}

func (o Operations) recordStep(ctx context.Context, telemetry TelemetryContext, step, stepResult string, duration time.Duration) {
	if step == "" {
		return
	}
	o.metricsProvider().RecordStep(ctx, telemetry.Environment, telemetry.Region, telemetry.Target, telemetry.Scenario, step, stepResult, duration)
}

func (o Operations) RecordStep(ctx context.Context, telemetry TelemetryContext, step, stepResult string, duration time.Duration) {
	o.recordStep(ctx, telemetry, step, stepResult, duration)
}

func (o Operations) RecordRun(ctx context.Context, telemetry TelemetryContext, runResult string, duration time.Duration) {
	o.metricsProvider().RecordRun(ctx, telemetry.Environment, telemetry.Region, telemetry.Target, telemetry.Scenario, runResult, duration)
}

func (o Operations) RecordOverlapSkip(ctx context.Context, telemetry TelemetryContext) {
	o.metricsProvider().RecordOverlapSkip(ctx, telemetry.Environment, telemetry.Region, telemetry.Target)
}

func (o Operations) RecordExecutionDelta(ctx context.Context, telemetry TelemetryContext, delta int64) {
	o.metricsProvider().RecordExecutionDelta(ctx, telemetry.Environment, telemetry.Region, telemetry.Target, telemetry.Scenario, delta)
}

func (o Operations) RecordCleanup(ctx context.Context, telemetry TelemetryContext, cleanupResult string) {
	o.metricsProvider().RecordCleanup(ctx, telemetry.Environment, telemetry.Region, telemetry.Target, cleanupResult)
}

func (o Operations) RecordRetainedSandbox(ctx context.Context, telemetry TelemetryContext, failedStep string) {
	o.metricsProvider().RecordRetainedSandbox(ctx, telemetry.Environment, telemetry.Region, telemetry.Target, failedStep)
}

func (o Operations) CreateSandbox(ctx context.Context, opts CreateSandboxOptions) (canaryapi.Sandbox, error) {
	start := o.now()
	sb, err := o.Client.CreateSandbox(ctx, opts.Request)
	o.recordStep(ctx, opts.Telemetry, "create_request", result(err), o.now().Sub(start))
	if err != nil {
		return canaryapi.Sandbox{}, fmt.Errorf("creating sandbox: %w", err)
	}
	return sb, nil
}

func (o Operations) WaitForStatus(ctx context.Context, sandboxID string, opts WaitForStatusOptions) error {
	deadline := o.now().Add(opts.Timeout)
	for {
		sb, err := o.Client.GetSandbox(ctx, sandboxID)
		if err != nil {
			return fmt.Errorf("fetching sandbox status: %w", err)
		}
		if sb.Status == opts.Want {
			return nil
		}
		if sb.Status == "failed" || sb.Status == "deleted" {
			return fmt.Errorf("sandbox entered terminal state %q", sb.Status)
		}
		if o.now().After(deadline) {
			return fmt.Errorf("timed out waiting for sandbox to become %s, last=%s", opts.Want, sb.Status)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for sandbox status %q: %w", opts.Want, ctx.Err())
		case <-time.After(opts.pollInterval()):
		}
	}
}

func (o Operations) WaitForStatusTimed(ctx context.Context, sandboxID string, opts WaitForStatusOptions) error {
	start := o.now()
	err := o.WaitForStatus(ctx, sandboxID, opts)
	o.recordStep(ctx, opts.Telemetry, opts.Step, result(err), o.now().Sub(start))
	return err
}

func (o Operations) WriteSandboxFile(ctx context.Context, sandboxID, accessToken, path string, content []byte) error {
	if err := o.Client.WriteFile(ctx, sandboxID, accessToken, path, content); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func (o Operations) WriteSandboxFileWithRetry(ctx context.Context, sandboxID, accessToken, path string, content []byte) error {
	delays := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second, 2 * time.Second}
	for attempt := 0; attempt <= len(delays); attempt++ {
		err := o.WriteSandboxFile(ctx, sandboxID, accessToken, path, content)
		if err == nil {
			return nil
		}
		if !isTransientWriteFileError(err) || attempt == len(delays) {
			return err
		}
		timer := time.NewTimer(delays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("writing %s: %w", path, ctx.Err())
		case <-timer.C:
		}
	}
	return nil
}

func isTransientWriteFileError(err error) bool {
	var statusErr *canaryapi.HTTPStatusError
	if !errors.As(err, &statusErr) {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return true
		}
		var netErr net.Error
		if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
			return true
		}
		lower := strings.ToLower(err.Error())
		switch {
		case strings.Contains(lower, "connection reset by peer"),
			strings.Contains(lower, "connection refused"),
			strings.Contains(lower, "broken pipe"),
			strings.Contains(lower, "use of closed network connection"),
			strings.Contains(lower, "eof"),
			strings.Contains(lower, "timeout"):
			return true
		default:
			return false
		}
	}
	switch statusErr.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (o Operations) Exec(ctx context.Context, sandboxID, accessToken string, req canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
	res, err := o.Client.Exec(ctx, sandboxID, accessToken, req)
	if err != nil {
		return res, err
	}
	return res, nil
}

func (o Operations) ExecStep(ctx context.Context, sandboxID, accessToken string, opts ExecStepOptions) (canaryapi.ExecResult, error) {
	start := o.now()
	res, err := o.Exec(ctx, sandboxID, accessToken, canaryapi.ExecRequest{
		Command:  opts.Command,
		TimeoutS: int(opts.Timeout.Seconds()),
	})
	if err == nil && res.ExitCode != 0 {
		err = ExecValidationError{ExitCode: res.ExitCode, Stderr: res.Stderr}
	}
	o.recordStep(ctx, opts.Telemetry, opts.Step, result(err), o.now().Sub(start))
	if err != nil {
		return res, fmt.Errorf("running %s step: %w", opts.Step, err)
	}
	return res, nil
}

func (o Operations) Pause(ctx context.Context, sandboxID string, telemetry TelemetryContext) error {
	start := o.now()
	err := o.Client.PauseSandbox(ctx, sandboxID)
	o.recordStep(ctx, telemetry, "pause_request", result(err), o.now().Sub(start))
	if err != nil {
		return fmt.Errorf("pausing sandbox: %w", err)
	}
	return nil
}

func (o Operations) Resume(ctx context.Context, sandboxID string, telemetry TelemetryContext) (canaryapi.ResumeResponse, error) {
	start := o.now()
	resp, err := o.Client.ResumeSandbox(ctx, sandboxID)
	if err == nil && resp.Status != "active" {
		err = fmt.Errorf("unexpected resume status %q", resp.Status)
	}
	o.recordStep(ctx, telemetry, "resume_request", result(err), o.now().Sub(start))
	if err != nil {
		return resp, fmt.Errorf("resuming sandbox: %w", err)
	}
	return resp, nil
}

func (o Operations) UploadVerificationUtilities(ctx context.Context, input VerificationUtilitiesInput) error {
	files := []string{
		"verification-utilities/verify_disk.sh",
		"verification-utilities/verify_memory.py",
	}
	start := o.now()
	var err error
	for _, name := range files {
		content, readErr := verificationUtilitiesFS.ReadFile(name)
		if readErr != nil {
			err = fmt.Errorf("read verification utility %s: %w", name, readErr)
			break
		}
		target := filepath.Join("/tmp/verification-utilities", filepath.Base(name))
		if writeErr := o.WriteSandboxFileWithRetry(ctx, input.SandboxID, input.AccessToken, target, content); writeErr != nil {
			err = fmt.Errorf("write verification utility %s: %w", name, writeErr)
			break
		}
	}
	o.recordStep(ctx, input.Telemetry, input.Step, result(err), o.now().Sub(start))
	if err != nil {
		return fmt.Errorf("preparing verification utilities: %w", err)
	}
	return nil
}

func (o Operations) PublishPreviewPort(ctx context.Context, sandboxID string, opts PublishPreviewPortOptions) error {
	start := o.now()
	err := o.Client.PublishPreviewPort(ctx, sandboxID, canaryapi.PublishPreviewPortRequest{
		Port:   opts.Port,
		Access: opts.Access,
	})
	o.recordStep(ctx, opts.Telemetry, "publish_preview_port", result(err), o.now().Sub(start))
	if err != nil {
		return fmt.Errorf("publishing preview port %d: %w", opts.Port, err)
	}
	return nil
}

func (o Operations) VerifyPreview(ctx context.Context, sandboxID, expected string, opts VerifyPreviewOptions) error {
	start := o.now()
	previewURL := o.Client.PreviewURL(sandboxID, opts.Port)
	ticker := time.NewTicker(opts.pollInterval())
	defer ticker.Stop()
	deadline := o.now().Add(opts.Timeout)

	var lastErr error
loop:
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, previewURL, nil)
		if err != nil {
			o.recordStep(ctx, opts.Telemetry, "preview", "failure", o.now().Sub(start))
			return StepError{Step: "resolve_preview_url", Err: fmt.Errorf("building preview request for %s: %w", previewURL, err)}
		}
		resp, err := o.httpClient().Do(req)
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
				o.recordStep(ctx, opts.Telemetry, "preview", "success", o.now().Sub(start))
				return nil
			}
		}
		if !o.now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
			break loop
		case <-ticker.C:
		}
	}
	o.recordStep(ctx, opts.Telemetry, "preview", "failure", o.now().Sub(start))
	return StepError{Step: "check_preview_url", Err: fmt.Errorf("verifying preview URL %s: %w", previewURL, lastErr)}
}

func (o Operations) RetainSandbox(ctx context.Context, resources RunResources, res RunResult, opts RetentionOptions, telemetry TelemetryContext) error {
	if err := o.Client.UpdateSandbox(ctx, resources.SandboxID, canaryapi.UpdateSandboxRequest{
		Metadata:          cloneMetadata(opts.Metadata),
		AutoDeleteSeconds: opts.AutoDeleteSeconds,
	}); err != nil {
		return fmt.Errorf("update retention metadata: %w", err)
	}
	o.RecordRetainedSandbox(ctx, telemetry, res.FailedStep)
	return nil
}

func (o Operations) FinalizeSandbox(ctx context.Context, resources RunResources, res RunResult, opts FinalizeOptions) (FinalizeResult, error) {
	if res.Err != nil && opts.Retain.Enabled {
		if err := o.RetainSandbox(ctx, resources, res, opts.Retain, opts.Telemetry); err != nil {
			return FinalizeResult{RetentionError: err}, res.Err
		}
		return FinalizeResult{Retained: true}, res.Err
	}
	if err := o.DeleteSandboxBestEffort(ctx, resources.SandboxID, opts.Delete); err != nil {
		if res.Err != nil {
			return FinalizeResult{DeleteError: err}, res.Err
		}
		return FinalizeResult{DeleteError: err}, err
	}
	return FinalizeResult{}, res.Err
}

func (o Operations) DeleteSandboxBestEffort(ctx context.Context, sandboxID string, opts DeleteSandboxOptions) error {
	start := o.now()
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	err := o.Client.DeleteSandbox(ctx, sandboxID)
	if errors.Is(err, canaryapi.ErrNotFound) {
		err = nil
	}
	o.RecordCleanup(ctx, opts.Telemetry, result(err))
	o.recordStep(ctx, opts.Telemetry, "delete_request", result(err), o.now().Sub(start))
	if err != nil {
		return fmt.Errorf("deleting sandbox: %w", err)
	}
	return nil
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (o VerifyPreviewOptions) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return 500 * time.Millisecond
}

func (o WaitForStatusOptions) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return 500 * time.Millisecond
}

type VerificationUtilitiesInput struct {
	SandboxID   string
	AccessToken string
	Step        string
	Telemetry   TelemetryContext
}
