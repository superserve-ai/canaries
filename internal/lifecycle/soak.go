package lifecycle

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

//go:embed soak_server.py
var soakServerScript []byte

const soakServerPath = "/tmp/canary-soak/server.py"

// These are optional capabilities so the other lifecycle clients retain their
// existing contract. The public API client implements all three.
type soakClient interface {
	ActivateSandbox(context.Context, string) (canaryapi.Sandbox, error)
	CreatePreviewToken(context.Context, string, int) (canaryapi.PreviewToken, error)
	DeletePreviewPort(context.Context, string, int) error
}

type soakIdentity struct {
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	StartedAt  string `json:"started_at"`
	Challenge  string `json:"challenge"`
}

func (i soakIdentity) valid() bool { return i.InstanceID != "" && i.PID > 0 && i.StartedAt != "" }

func (i soakIdentity) same(other soakIdentity) bool {
	return i.InstanceID == other.InstanceID && i.PID == other.PID && i.StartedAt == other.StartedAt
}

var soakOperations = []struct{ key, label string }{
	{"status", "Status checks"},
	{"activate", "Sandbox connections"},
	{"connect", "Original preview connections"},
	{"preview_create", "Preview URL creation"},
	{"preview_token", "Preview token creation"},
	{"preview_connect", "Temporary preview connections"},
	{"preview_auth", "Unauthenticated access rejection"},
	{"preview_delete", "Preview URL deletion"},
	{"preview_revoked", "Deleted preview access rejection"},
	{"short_task", "Short tasks"},
	{"process_identity", "Original process checks"},
	{"pause", "Deliberate pause"},
}

type soakCounter struct {
	Attempts int
	Failures int
	Skipped  int
	Duration time.Duration
	Maximum  time.Duration
}

type soakSummary struct {
	Started       time.Time
	Elapsed       time.Duration
	Counters      map[string]*soakCounter
	FirstError    error
	FailedStep    string
	IdlePauses    int
	MissedChecks  int
	Cycles        int
	PauseVerified bool
	SeenActive    bool
	Cleanup       string
	ExpiresAt     time.Time
}

func (s *soakSummary) counter(operation string) *soakCounter {
	if s.Counters == nil {
		s.Counters = make(map[string]*soakCounter)
	}
	if s.Counters[operation] == nil {
		s.Counters[operation] = &soakCounter{}
	}
	return s.Counters[operation]
}

func (r Runner) soakAttempt(ctx context.Context, s *soakSummary, operation string, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		r.soakSkip(ctx, s, operation)
		return err
	}
	start := r.Clock()
	// Each operation is bounded even if the injected HTTP client has no timeout.
	timeout := r.Config.HTTPTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if operation == "short_task" || operation == "pause" {
		timeout = r.Config.CommandTimeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := fn(attemptCtx)
	c := s.counter(operation)
	c.Attempts++
	duration := r.Clock().Sub(start)
	c.Duration += duration
	if duration > c.Maximum {
		c.Maximum = duration
	}
	if err != nil {
		c.Failures++
		if s.FirstError == nil {
			s.FirstError, s.FailedStep = err, "soak_"+operation
		}
		log.Warn().Err(err).Str("operation", operation).Msg("soak check failed")
	}
	r.operations().RecordStep(ctx, r.telemetry(), "soak_"+operation, result(err), duration)
	return err
}

func (r Runner) soakSkip(ctx context.Context, s *soakSummary, operations ...string) {
	for _, operation := range operations {
		s.counter(operation).Skipped++
		r.operations().RecordStep(ctx, r.telemetry(), "soak_"+operation, "skipped", 0)
	}
}

func (r Runner) runSoak(ctx context.Context, runID string) (res RunResult) {
	summary := soakSummary{Started: r.Clock(), Cleanup: "no sandbox created"}
	resources := RunResources{RunID: runID, CreatedAt: r.Clock().UTC()}
	defer func() {
		summary.Elapsed = r.Clock().Sub(summary.Started)
		if res.Err == nil && summary.FirstError != nil {
			res.Err, res.FailedStep = summary.FirstError, summary.FailedStep
		}
		if resources.SandboxID != "" {
			// Soak never retains a failed sandbox. Its original, bounded expiry
			// remains intact if deletion fails or the process is killed.
			cleanupErr := r.operations().DeleteSandboxBestEffort(context.Background(), resources.SandboxID, DeleteSandboxOptions{
				Timeout: r.Config.DeleteTimeout, Telemetry: r.telemetry(),
			})
			if cleanupErr != nil {
				summary.Cleanup = "failed; janitor eligible after " + summary.ExpiresAt.UTC().Format(time.RFC3339)
				res.Err = errors.Join(res.Err, cleanupErr)
				if res.FailedStep == "" {
					res.FailedStep = "delete_request"
				}
			} else {
				summary.Cleanup = "sandbox removed"
			}
		}
		res.Report = summary.report(r.Config.Target, res)
	}()

	client, ok := r.Client.(soakClient)
	if !ok {
		res.Err, res.FailedStep = errors.New("client lacks soak API capabilities"), "soak_setup"
		return res
	}
	if r.Config.SoakDuration <= 0 || r.Config.SoakInterval <= 0 {
		res.Err, res.FailedStep = errors.New("soak duration and interval must be positive"), "soak_setup"
		return res
	}
	request := r.canaryCreateSandboxRequest(resources)
	summary.ExpiresAt = resources.CreatedAt.Add(r.Config.ResourceTTL)
	idleSeconds := 60
	request.PreviewAccess = "private"
	request.IdleTimeoutSeconds = &idleSeconds
	request.TimeoutSeconds = int(r.Config.RunTimeout.Seconds())
	request.AutoDeleteSeconds = int(r.Config.ResourceTTL.Seconds())
	request.Metadata[sandboxmetadata.KeyExpiresAt] = summary.ExpiresAt.Format(time.RFC3339)
	request.Metadata["scenario"] = "soak"
	sb, err := r.operations().CreateSandbox(ctx, CreateSandboxOptions{Request: request, Telemetry: r.telemetry()})
	if err != nil {
		res.Err, res.FailedStep = err, "create_request"
		return res
	}
	resources.SandboxID, res.SandboxID = sb.ID, sb.ID
	if err := r.operations().WaitForStatusTimed(ctx, sb.ID, WaitForStatusOptions{
		Want: "active", Step: "create_wait_active", PollInterval: r.Config.PollInterval,
		Timeout: r.Config.CommandTimeout, Telemetry: r.telemetry(),
	}); err != nil {
		res.Err, res.FailedStep = err, "create_wait_active"
		return res
	}
	identity, err := r.startSoakServer(ctx, sb)
	if err != nil {
		res.Err, res.FailedStep = err, "soak_start_server"
		return res
	}
	if err := r.Client.PublishPreviewPort(ctx, sb.ID, canaryapi.PublishPreviewPortRequest{Port: r.Config.PreviewPort, Access: "private"}); err != nil {
		res.Err, res.FailedStep = err, "soak_original_preview"
		return res
	}
	originalToken, err := client.CreatePreviewToken(ctx, sb.ID, r.Config.PreviewPort)
	if err != nil {
		res.Err, res.FailedStep = err, "soak_original_preview_token"
		return res
	}
	// Minute probes may keep a 60s idle timer alive. One explicit pause ensures
	// the run proves restore survival; later idle pauses are observed separately.
	if err := r.soakAttempt(ctx, &summary, "pause", func(ctx context.Context) error {
		if err := r.Client.PauseSandbox(ctx, sb.ID); err != nil {
			return err
		}
		return r.operations().WaitForStatus(ctx, sb.ID, WaitForStatusOptions{
			Want: "paused", PollInterval: r.Config.PollInterval, Timeout: r.Config.CommandTimeout,
		})
	}); err != nil && ctx.Err() != nil {
		res.Err, res.FailedStep = ctx.Err(), "soak_pause"
		return res
	}

	summary.Started = r.Clock()
	deadline := summary.Started.Add(r.Config.SoakDuration)
	next := summary.Started
	for {
		if err := r.waitSoak(ctx, next.Sub(r.Clock())); err != nil {
			res.Err, res.FailedStep = err, "soak_wait"
			return res
		}
		if ctx.Err() != nil {
			res.Err, res.FailedStep = ctx.Err(), "soak_wait"
			return res
		}
		r.soakCycle(ctx, client, &sb, originalToken, identity, &summary)
		summary.Cycles++
		if err := ctx.Err(); err != nil {
			res.Err, res.FailedStep = err, "soak_check"
			return res
		}
		if !r.Clock().Before(deadline) {
			break
		}
		next = next.Add(r.Config.SoakInterval)
		if now := r.Clock(); next.Before(now) {
			missed := int(now.Sub(next)/r.Config.SoakInterval) + 1
			summary.MissedChecks += missed
			next = next.Add(time.Duration(missed) * r.Config.SoakInterval)
		}
		if next.After(deadline) {
			next = deadline
		}
	}
	if !summary.PauseVerified && summary.FirstError == nil {
		res.Err, res.FailedStep = errors.New("original process was not verified after pause"), "soak_process_identity"
	} else if summary.MissedChecks > 0 && summary.FirstError == nil {
		res.Err, res.FailedStep = fmt.Errorf("missed %d scheduled soak check intervals", summary.MissedChecks), "soak_schedule"
	}
	return res
}

func (r Runner) waitSoak(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	if r.Wait != nil {
		return r.Wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r Runner) startSoakServer(ctx context.Context, sb canaryapi.Sandbox) (soakIdentity, error) {
	if err := r.operations().WriteSandboxFileWithRetry(ctx, sb.ID, sb.AccessToken, soakServerPath, soakServerScript); err != nil {
		return soakIdentity{}, err
	}
	// A new session and detached standard streams keep the server independent
	// of the short command that starts it.
	command := fmt.Sprintf(`python3 -c 'import subprocess,sys; f=open("/tmp/canary-soak/server.log","ab"); subprocess.Popen([sys.executable,"%s","%d","%d"],stdin=subprocess.DEVNULL,stdout=f,stderr=f,start_new_session=True)'`, soakServerPath, r.Config.PreviewPort, r.Config.PreviewPort+1)
	if _, err := r.operations().ExecStep(ctx, sb.ID, sb.AccessToken, ExecStepOptions{
		Step: "soak_start_server", Command: command, Timeout: r.Config.CommandTimeout, Telemetry: r.telemetry(),
	}); err != nil {
		return soakIdentity{}, err
	}
	command = fmt.Sprintf(`python3 -c 'import json,time,urllib.request
for attempt in range(100):
 try:
  a=json.load(urllib.request.urlopen("http://127.0.0.1:%d",timeout=1)); b=json.load(urllib.request.urlopen("http://127.0.0.1:%d",timeout=1))
  assert a==b
  print(json.dumps(a)); break
 except Exception:
  if attempt==99: raise
  time.sleep(0.1)'`, r.Config.PreviewPort, r.Config.PreviewPort+1)
	out, err := r.operations().ExecStep(ctx, sb.ID, sb.AccessToken, ExecStepOptions{
		Step: "soak_server_ready", Command: command, Timeout: r.Config.CommandTimeout, Telemetry: r.telemetry(),
	})
	var identity soakIdentity
	if err == nil {
		err = json.Unmarshal([]byte(out.Stdout), &identity)
	}
	if err == nil && !identity.valid() {
		err = errors.New("server returned an invalid startup identity")
	}
	return identity, err
}

func (r Runner) soakCycle(ctx context.Context, client soakClient, sb *canaryapi.Sandbox, originalToken canaryapi.PreviewToken, identity soakIdentity, s *soakSummary) {
	r.soakAttempt(ctx, s, "status", func(ctx context.Context) error {
		current, err := r.Client.GetSandbox(ctx, sb.ID)
		if err == nil && current.Status == "paused" && s.SeenActive {
			s.IdlePauses++
			s.SeenActive = false
		}
		return err
	})
	active := r.soakAttempt(ctx, s, "activate", func(ctx context.Context) error {
		current, err := client.ActivateSandbox(ctx, sb.ID)
		if err == nil && (current.ID != sb.ID || current.Status != "active" || current.AccessToken == "") {
			err = errors.New("activation did not return the original active sandbox and access token")
		}
		if err == nil {
			*sb = current
			s.SeenActive = true
		}
		return err
	})
	// Preview checks remain actual attempts even if activation failed; they may
	// expose a different failure mode and never trigger an implicit retry.
	r.soakAttempt(ctx, s, "connect", func(ctx context.Context) error {
		return r.checkSoakPreview(ctx, sb.ID, r.Config.PreviewPort, originalToken, identity, http.StatusOK)
	})
	churnPort := r.Config.PreviewPort + 1
	created := r.soakAttempt(ctx, s, "preview_create", func(ctx context.Context) error {
		return r.Client.PublishPreviewPort(ctx, sb.ID, canaryapi.PublishPreviewPortRequest{Port: churnPort, Access: "private"})
	})
	var token canaryapi.PreviewToken
	var tokenErr error
	if created == nil {
		tokenErr = r.soakAttempt(ctx, s, "preview_token", func(ctx context.Context) error {
			var err error
			token, err = client.CreatePreviewToken(ctx, sb.ID, churnPort)
			return err
		})
		if tokenErr == nil {
			r.soakAttempt(ctx, s, "preview_connect", func(ctx context.Context) error {
				return r.checkSoakPreview(ctx, sb.ID, churnPort, token, identity, http.StatusOK)
			})
		} else {
			r.soakSkip(ctx, s, "preview_connect")
		}
		r.soakAttempt(ctx, s, "preview_auth", func(ctx context.Context) error {
			return r.checkSoakPreview(ctx, sb.ID, churnPort, canaryapi.PreviewToken{}, identity, http.StatusUnauthorized)
		})
	} else {
		r.soakSkip(ctx, s, "preview_token", "preview_connect", "preview_auth")
	}
	// Delete even after an ambiguous create failure: the server may have
	// published the port before its response was lost.
	deleted := r.soakAttempt(ctx, s, "preview_delete", func(ctx context.Context) error {
		return client.DeletePreviewPort(ctx, sb.ID, churnPort)
	})
	if deleted == nil && created == nil && tokenErr == nil {
		r.soakAttempt(ctx, s, "preview_revoked", func(ctx context.Context) error {
			return r.checkSoakPreview(ctx, sb.ID, churnPort, token, identity, http.StatusNotFound)
		})
	} else {
		r.soakSkip(ctx, s, "preview_revoked")
	}
	if active != nil {
		r.soakSkip(ctx, s, "short_task", "process_identity")
		return
	}
	challenge := uuid.NewString()
	left, right := int(challenge[0])+s.Cycles, int(challenge[1])+s.Cycles
	var current soakIdentity
	err := r.soakAttempt(ctx, s, "short_task", func(ctx context.Context) error {
		command := fmt.Sprintf(`python3 -c 'import json,urllib.request; print(json.dumps({"value":%d+%d,"server":json.load(urllib.request.urlopen("http://127.0.0.1:%d/?challenge=%s",timeout=5))}))'`, left, right, r.Config.PreviewPort, challenge)
		out, err := r.Client.Exec(ctx, sb.ID, sb.AccessToken, canaryapi.ExecRequest{Command: command, TimeoutS: int(r.Config.CommandTimeout.Seconds())})
		if err != nil {
			return err
		}
		if out.ExitCode != 0 {
			return ExecValidationError{ExitCode: out.ExitCode, Stderr: out.Stderr}
		}
		var response struct {
			Value  int          `json:"value"`
			Server soakIdentity `json:"server"`
		}
		if err := json.Unmarshal([]byte(out.Stdout), &response); err != nil {
			return err
		}
		if response.Value != left+right || response.Server.Challenge != challenge || !response.Server.valid() {
			return errors.New("short task returned unexpected computation or server response")
		}
		current = response.Server
		return nil
	})
	if err != nil {
		r.soakSkip(ctx, s, "process_identity")
		return
	}
	r.soakAttempt(ctx, s, "process_identity", func(context.Context) error {
		if !identity.same(current) {
			return errors.New("original server process identity changed")
		}
		if s.counter("pause").Attempts == 1 && s.counter("pause").Failures == 0 {
			s.PauseVerified = true
		}
		return nil
	})
}

func (r Runner) checkSoakPreview(ctx context.Context, sandboxID string, port int, token canaryapi.PreviewToken, identity soakIdentity, wantStatus int) error {
	ctx, cancel := context.WithTimeout(ctx, r.Config.PreviewTimeout)
	defer cancel()
	challenge := uuid.NewString()
	endpoint := r.Client.PreviewURL(sandboxID, port) + "/?challenge=" + url.QueryEscape(challenge)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if token.Token != "" {
		if token.Header != "X-Superserve-Preview-Token" {
			return errors.New("unexpected private preview authentication header")
		}
		req.Header.Set(token.Header, token.Token)
	} else if wantStatus == http.StatusOK {
		return errors.New("private preview token is missing")
	}
	req.Header.Set("Cache-Control", "no-cache")
	client := *r.httpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("preview returned HTTP %d, expected %d", resp.StatusCode, wantStatus)
	}
	if wantStatus != http.StatusOK {
		return nil
	}
	var current soakIdentity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&current); err != nil {
		return err
	}
	if !identity.same(current) || current.Challenge != challenge {
		return errors.New("preview returned a different process identity or stale response")
	}
	return nil
}

func (s *soakSummary) report(target string, res RunResult) string {
	status := "passed"
	if res.Err != nil {
		status = "failed"
	}
	var report strings.Builder
	fmt.Fprintf(&report, "*Soak test* · %s\n%s · %s · %d check cycles\n", target, s.Elapsed.Round(time.Second), status, s.Cycles)
	for _, operation := range soakOperations {
		c := s.counter(operation.key)
		fmt.Fprintf(&report, "%s: %d/%d failed", operation.label, c.Failures, c.Attempts)
		if c.Attempts > 0 {
			fmt.Fprintf(&report, " (%.1f%%)", 100*float64(c.Failures)/float64(c.Attempts))
		}
		if c.Skipped > 0 {
			fmt.Fprintf(&report, "; %d skipped", c.Skipped)
		}
		if c.Attempts > 0 && (operation.key == "connect" || operation.key == "activate") {
			fmt.Fprintf(&report, "; avg %s, max %s", (c.Duration / time.Duration(c.Attempts)).Round(time.Millisecond), c.Maximum.Round(time.Millisecond))
		}
		report.WriteByte('\n')
	}
	identity := s.counter("process_identity")
	fmt.Fprintf(&report, "Same process: %d verified, %d changed, %d unknown\n", identity.Attempts-identity.Failures, identity.Failures, identity.Skipped)
	fmt.Fprintf(&report, "Deliberate pause/resume verified: %t · observed idle pauses: %d · missed intervals: %d\n", s.PauseVerified, s.IdlePauses, s.MissedChecks)
	fmt.Fprintf(&report, "Cleanup: %s\n", s.Cleanup)
	if res.SandboxID != "" {
		fmt.Fprintf(&report, "Sandbox: `%s`\n", res.SandboxID)
	}
	if res.Err != nil {
		fmt.Fprintf(&report, "Failed step: `%s`", res.FailedStep)
	}
	return strings.TrimSpace(report.String())
}
