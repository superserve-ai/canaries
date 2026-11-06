package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

func TestSoakRunsSixHoursWithOneFinalSummary(t *testing.T) {
	f := newSoakFixture(t)
	f.runner.Config.SoakDuration = 6 * time.Hour
	started := f.now
	if err := f.runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := f.now.Sub(started); elapsed != 6*time.Hour {
		t.Fatalf("elapsed = %s, want six hours", elapsed)
	}
	if f.activateCalls != 361 || f.shortCalls != 361 || f.startCalls != 1 || f.pauseCalls != 1 || f.deleteCalls != 1 {
		t.Fatalf("calls: activate=%d short=%d start=%d pause=%d delete=%d", f.activateCalls, f.shortCalls, f.startCalls, f.pauseCalls, f.deleteCalls)
	}
	if f.tokenCalls[18080] != 1 || f.tokenCalls[18081] != 361 || f.previewDeletes != 361 {
		t.Fatalf("persistent/churn token calls=%v and deletes=%d", f.tokenCalls, f.previewDeletes)
	}
	f.wantSummary("6h0m0s · passed · 361 check cycles", "Original preview connections: 0/361 failed", "Same process: 361 verified, 0 changed, 0 unknown", "Deliberate pause/resume verified: true")
	if f.slackAfterCycles != 361 || f.lock.key != "staging-us-central1-soak" || !f.lock.released {
		t.Fatalf("final summary ran at cycle %d; lock key=%q released=%t", f.slackAfterCycles, f.lock.key, f.lock.released)
	}
	if got := f.steps.count("soak_connect", "success"); got != 361 {
		t.Fatalf("connect success metric = %d, want 361", got)
	}
	if f.steps.runScenario != "soak" || f.steps.runResult != "success" {
		t.Fatalf("run metric = %s/%s", f.steps.runScenario, f.steps.runResult)
	}
	if len(f.creates) != 1 {
		t.Fatalf("created %d sandboxes", len(f.creates))
	}
	request := f.creates[0]
	if request.PreviewAccess != "private" || request.IdleTimeoutSeconds == nil || *request.IdleTimeoutSeconds != 60 || request.Metadata["scenario"] != "soak" {
		t.Fatalf("missing private preview or soak ownership configuration: %+v", request)
	}
	if request.AutoDeleteSeconds < int((6 * time.Hour).Seconds()) {
		t.Fatalf("auto-delete can remove the sandbox during the run: %d", request.AutoDeleteSeconds)
	}
	expiry, err := time.Parse(time.RFC3339, request.Metadata[sandboxmetadata.KeyExpiresAt])
	if err != nil || !expiry.After(f.now) {
		t.Fatalf("cleanup protection expires during the soak: %q, %v", request.Metadata[sandboxmetadata.KeyExpiresAt], err)
	}
}

func TestSoakCountsIntermittentFailuresAndCompletesObservation(t *testing.T) {
	f := newSoakFixture(t)
	f.runner.Config.SoakDuration = 350 * time.Minute
	f.previewFailure = func(port, attempt int) bool { return port == 18080 && attempt <= 15 }
	if err := f.runner.Run(context.Background()); err == nil {
		t.Fatal("intermittent failures must fail the run")
	}
	if f.activateCalls != 351 || f.shortCalls != 351 || f.deleteCalls != 1 {
		t.Fatalf("stopped early: activate=%d short=%d delete=%d", f.activateCalls, f.shortCalls, f.deleteCalls)
	}
	f.wantSummary("failed · 351 check cycles", "Original preview connections: 15/351 failed (4.3%)", "Same process: 351 verified, 0 changed, 0 unknown")
	if got := f.steps.count("soak_connect", "failure"); got != 15 {
		t.Fatalf("connect failure metric = %d, want 15", got)
	}
}

func TestSoakSeparatesSkippedChecksFromAttemptsAndDeletesAmbiguousPublication(t *testing.T) {
	f := newSoakFixture(t)
	f.publishFailure = func(attempt int) error {
		if attempt == 1 {
			return errors.New("publication response lost")
		}
		return nil
	}
	if err := f.runner.Run(context.Background()); err == nil {
		t.Fatal("publication failure must fail the run")
	}
	f.wantSummary("Preview URL creation: 1/3 failed", "Preview token creation: 0/2 failed (0.0%); 1 skipped", "Temporary preview connections: 0/2 failed (0.0%); 1 skipped", "Preview URL deletion: 0/3 failed", "Deleted preview access rejection: 0/2 failed (0.0%); 1 skipped")
	if f.previewDeletes != 3 || f.steps.count("soak_preview_connect", "skipped") != 1 || f.tokenCalls[18081] != 2 {
		t.Fatalf("ambiguous create cleanup/dependencies wrong: deletes=%d skipped=%d token calls=%v", f.previewDeletes, f.steps.count("soak_preview_connect", "skipped"), f.tokenCalls)
	}
}

func TestSoakDetectsReplacementProcessWithoutRestartingIt(t *testing.T) {
	f := newSoakFixture(t)
	f.onActivate = func(attempt int) error {
		if attempt == 2 {
			f.identity.InstanceID = "replacement-process"
		}
		return nil
	}
	if err := f.runner.Run(context.Background()); err == nil {
		t.Fatal("replaced process must fail the run")
	}
	f.wantSummary("Same process: 1 verified, 2 changed, 0 unknown", "Original process checks: 2/3 failed")
	if f.startCalls != 1 || f.shortCalls != 3 {
		t.Fatalf("server restarted or observation stopped: starts=%d short=%d", f.startCalls, f.shortCalls)
	}
}

func TestSoakUnreachableProcessIsUnknownRatherThanChanged(t *testing.T) {
	f := newSoakFixture(t)
	f.shortFailure = func(int) error { return errors.New("local server unreachable") }
	f.previewFailure = func(port, attempt int) bool { return port == 18080 }
	if err := f.runner.Run(context.Background()); err == nil {
		t.Fatal("unreachable process must fail the run")
	}
	f.wantSummary("Same process: 0 verified, 0 changed, 3 unknown", "Original process checks: 0/0 failed; 3 skipped", "Short tasks: 3/3 failed")
	if f.steps.count("soak_process_identity", "failure") != 0 || f.steps.count("soak_process_identity", "skipped") != 3 {
		t.Fatal("unreachable process was misclassified as a changed process")
	}
}

func TestSoakObservesIdlePauseAndRefreshesCredentialsBeforeChecks(t *testing.T) {
	f := newSoakFixture(t)
	wait := f.runner.Wait
	f.runner.Wait = func(ctx context.Context, delay time.Duration) error {
		f.status = "paused"
		return wait(ctx, delay)
	}
	if err := f.runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.wantSummary("Deliberate pause/resume verified: true · observed idle pauses: 2", "Same process: 3 verified, 0 changed, 0 unknown")
	if f.pausedActivations != 3 {
		t.Fatalf("only %d activations resumed a paused sandbox, want 3", f.pausedActivations)
	}
}

func TestSoakInitialActivationFailuresDoNotCountAsIdlePauses(t *testing.T) {
	f := newSoakFixture(t)
	f.onActivate = func(attempt int) error {
		if attempt <= 2 {
			return errors.New("initial resume unavailable")
		}
		return nil
	}
	if err := f.runner.Run(context.Background()); err == nil {
		t.Fatal("activation failure must fail the run")
	}
	f.wantSummary("Sandbox connections: 2/3 failed", "Same process: 1 verified, 0 changed, 2 unknown", "observed idle pauses: 0", "Short tasks: 0/1 failed (0.0%); 2 skipped")
	if f.activateCalls != 3 || f.shortCalls != 1 || f.previewDeletes != 3 {
		t.Fatalf("later checks stopped: activates=%d short=%d deletes=%d", f.activateCalls, f.shortCalls, f.previewDeletes)
	}
}

func TestSoakRepeatedFailedResumeCountsOneIdlePause(t *testing.T) {
	f := newSoakFixture(t)
	f.runner.Config.SoakDuration = 3 * time.Minute
	wait := f.runner.Wait
	f.runner.Wait = func(ctx context.Context, delay time.Duration) error {
		f.status = "paused"
		return wait(ctx, delay)
	}
	f.onActivate = func(attempt int) error {
		if attempt == 2 || attempt == 3 {
			return errors.New("resume still unavailable")
		}
		return nil
	}
	if err := f.runner.Run(context.Background()); err == nil {
		t.Fatal("failed resume attempts must fail the run")
	}
	f.wantSummary("failed · 4 check cycles", "Sandbox connections: 2/4 failed", "Same process: 2 verified, 0 changed, 2 unknown", "observed idle pauses: 1")
	if f.activateCalls != 4 || f.pausedActivations != 2 {
		t.Fatalf("activation attempts=%d successful paused activations=%d", f.activateCalls, f.pausedActivations)
	}
}

func TestSoakMissedCadenceFailsWithoutBurstingCatchupChecks(t *testing.T) {
	f := newSoakFixture(t)
	f.onActivate = func(attempt int) error {
		if attempt == 1 {
			f.now = f.now.Add(95 * time.Second)
		}
		return nil
	}
	if err := f.runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "missed 1 scheduled soak check intervals") {
		t.Fatalf("error = %v, want missed cadence failure", err)
	}
	f.wantSummary("2m0s · failed · 2 check cycles", "missed intervals: 1", "Failed step: `soak_schedule`")
	if f.activateCalls != 2 || f.shortCalls != 2 || f.deleteCalls != 1 {
		t.Fatalf("missed tick was replayed or cleanup skipped: activate=%d short=%d delete=%d", f.activateCalls, f.shortCalls, f.deleteCalls)
	}
}

func TestSoakDeadlineAccountsForUnexecutedScheduledChecks(t *testing.T) {
	tests := []struct {
		name          string
		duration      time.Duration
		slowCycle     int
		cycleDuration time.Duration
		cycles        int
		missed        int
	}{
		{"last minute overruns deadline", 6 * time.Hour, 360, 70 * time.Second, 360, 1},
		{"last minute finishes exactly at deadline", 6 * time.Hour, 360, time.Minute, 360, 1},
		{"first cycle overruns whole duration", 6 * time.Hour, 1, 6*time.Hour + 10*time.Second, 1, 360},
		{"overrun includes clamped final check", 150 * time.Second, 2, 100 * time.Second, 2, 2},
		{"large overrun stops counting at clamped deadline", 150 * time.Second, 1, time.Hour, 1, 3},
		{"final scheduled check may finish late", 2 * time.Minute, 3, 70 * time.Second, 3, 0},
		{"clamped final check may finish late", 150 * time.Second, 4, 70 * time.Second, 4, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSoakFixture(t)
			f.runner.Config.SoakDuration = tc.duration
			f.onActivate = func(attempt int) error {
				if attempt == tc.slowCycle {
					f.now = f.now.Add(tc.cycleDuration)
				}
				return nil
			}
			err := f.runner.Run(context.Background())
			status := "passed"
			if tc.missed > 0 {
				status = "failed"
				want := fmt.Sprintf("missed %d scheduled soak check intervals", tc.missed)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want %q", err, want)
				}
			} else if err != nil {
				t.Fatalf("the actual final check was completed: %v", err)
			}
			f.wantSummary(
				fmt.Sprintf("%s · %d check cycles", status, tc.cycles),
				fmt.Sprintf("missed intervals: %d", tc.missed),
				fmt.Sprintf("Original preview connections: 0/%d failed", tc.cycles),
			)
			if f.activateCalls != tc.cycles || f.shortCalls != tc.cycles || f.previewDeletes != tc.cycles || f.deleteCalls != 1 {
				t.Fatalf("actual attempts/cleanup changed: activate=%d short=%d previews=%d cleanup=%d", f.activateCalls, f.shortCalls, f.previewDeletes, f.deleteCalls)
			}
		})
	}
}

func TestSoakCancellationDuringCycleDoesNotInventAttempts(t *testing.T) {
	f := newSoakFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.previewFailure = func(port, attempt int) bool {
		if port == 18080 {
			cancel()
			return true
		}
		return false
	}
	if err := f.runner.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	f.wantSummary("Preview URL creation: 0/0 failed; 1 skipped", "Preview URL deletion: 0/0 failed; 1 skipped", "Short tasks: 0/0 failed; 1 skipped", "Same process: 0 verified, 0 changed, 1 unknown")
	if f.churnCreates != 0 || f.previewDeletes != 0 || f.shortCalls != 0 || f.deleteCalls != 1 {
		t.Fatalf("operation ran after cancellation or cleanup missed: creates=%d previews=%d short=%d delete=%d", f.churnCreates, f.previewDeletes, f.shortCalls, f.deleteCalls)
	}
}

func TestSoakCancellationDuringFinalCycleCannotReportSuccess(t *testing.T) {
	f := newSoakFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onActivate = func(attempt int) error {
		if attempt == 3 {
			cancel()
		}
		return nil
	}
	if err := f.runner.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation during final cycle", err)
	}
	f.wantSummary("2m0s · failed · 3 check cycles", "Same process: 2 verified, 0 changed, 1 unknown", "Failed step: `soak_check`")
	if f.deleteCalls != 1 || f.shortCalls != 2 || f.steps.runResult != "failure" {
		t.Fatalf("final cancellation: delete=%d short=%d run metric=%s", f.deleteCalls, f.shortCalls, f.steps.runResult)
	}
}

func TestSoakCancellationStillCleansUpAndSendsFinalSummary(t *testing.T) {
	f := newSoakFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.runner.Config.RetainFailedSandbox = true
	f.runner.Wait = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	if err := f.runner.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if f.deleteCalls != 1 || f.cleanupContextErr != nil || f.retentionCalls != 0 {
		t.Fatalf("canceled cleanup: deletes=%d context=%v retention=%d", f.deleteCalls, f.cleanupContextErr, f.retentionCalls)
	}
	f.wantSummary("failed · 1 check cycles", "Failed step: `soak_wait`")
}

func TestSoakCleanupFailureAppearsInFinalSummary(t *testing.T) {
	f := newSoakFixture(t)
	f.deleteFailure = errors.New("sandbox deletion rejected")
	if err := f.runner.Run(context.Background()); !errors.Is(err, f.deleteFailure) {
		t.Fatalf("error = %v, want deletion failure", err)
	}
	f.wantSummary("failed · 3 check cycles", "Failed step: `delete_request`")
	if f.deleteCalls != 1 || f.retentionCalls != 0 || f.steps.cleanupResult != "failure" {
		t.Fatalf("delete calls=%d retention=%d cleanup metric=%s", f.deleteCalls, f.retentionCalls, f.steps.cleanupResult)
	}
}

type soakFakeClient struct {
	*fakeClient
	activateFn      func(context.Context, string) (canaryapi.Sandbox, error)
	tokenFn         func(context.Context, string, int) (canaryapi.PreviewToken, error)
	deletePreviewFn func(context.Context, string, int) error
}

func (c *soakFakeClient) ActivateSandbox(ctx context.Context, id string) (canaryapi.Sandbox, error) {
	return c.activateFn(ctx, id)
}

func (c *soakFakeClient) CreatePreviewToken(ctx context.Context, id string, port int) (canaryapi.PreviewToken, error) {
	return c.tokenFn(ctx, id, port)
}

func (c *soakFakeClient) DeletePreviewPort(ctx context.Context, id string, port int) error {
	return c.deletePreviewFn(ctx, id, port)
}

type soakFixture struct {
	t                 *testing.T
	runner            Runner
	now               time.Time
	identity          soakIdentity
	status            string
	client            *soakFakeClient
	steps             *soakMetrics
	lock              *soakLock
	creates           []canaryapi.CreateSandboxRequest
	published         map[int]bool
	tokens            map[int]string
	tokenCalls        map[int]int
	previewAttempts   map[int]int
	activateCalls     int
	pausedActivations int
	shortCalls        int
	startCalls        int
	pauseCalls        int
	deleteCalls       int
	previewDeletes    int
	churnCreates      int
	retentionCalls    int
	cleanupContextErr error
	slack             []string
	slackAfterCycles  int
	onActivate        func(int) error
	publishFailure    func(int) error
	shortFailure      func(int) error
	previewFailure    func(port, attempt int) bool
	deleteFailure     error
}

var soakValuePattern = regexp.MustCompile(`"value":([0-9]+)\+([0-9]+)`)
var soakChallengePattern = regexp.MustCompile(`challenge=([a-f0-9-]+)`)

func newSoakFixture(t *testing.T) *soakFixture {
	t.Helper()
	f := &soakFixture{
		t: t, now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), status: "active",
		identity:  soakIdentity{InstanceID: "original-process", PID: 42, StartedAt: "startup-time"},
		published: make(map[int]bool), tokens: make(map[int]string), tokenCalls: make(map[int]int), previewAttempts: make(map[int]int),
		steps: &soakMetrics{}, lock: &soakLock{},
	}
	f.client = &soakFakeClient{fakeClient: &fakeClient{}}
	f.client.createSandboxFn = func(ctx context.Context, request canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
		f.creates = append(f.creates, request)
		return canaryapi.Sandbox{ID: "sb-soak", Status: "active", AccessToken: "initial-token"}, ctx.Err()
	}
	f.client.getSandboxFn = func(ctx context.Context, id string) (canaryapi.Sandbox, error) {
		return canaryapi.Sandbox{ID: id, Status: f.status}, ctx.Err()
	}
	f.client.pauseSandboxFn = func(ctx context.Context, id string) error {
		f.pauseCalls++
		f.status = "paused"
		return ctx.Err()
	}
	f.client.activateFn = func(ctx context.Context, id string) (canaryapi.Sandbox, error) {
		f.activateCalls++
		if f.onActivate != nil {
			if err := f.onActivate(f.activateCalls); err != nil {
				return canaryapi.Sandbox{}, err
			}
		}
		if f.status == "paused" {
			f.pausedActivations++
		}
		f.status = "active"
		return canaryapi.Sandbox{ID: id, Status: "active", AccessToken: fmt.Sprintf("token-%d", f.activateCalls)}, ctx.Err()
	}
	f.client.publishPreviewPortFn = func(ctx context.Context, id string, request canaryapi.PublishPreviewPortRequest) error {
		if request.Access != "private" {
			t.Errorf("port %d published without authentication", request.Port)
		}
		f.published[request.Port] = true
		if request.Port == 18081 {
			f.churnCreates++
			if f.publishFailure != nil {
				return f.publishFailure(f.churnCreates)
			}
		}
		return ctx.Err()
	}
	f.client.tokenFn = func(ctx context.Context, id string, port int) (canaryapi.PreviewToken, error) {
		f.tokenCalls[port]++
		f.tokens[port] = fmt.Sprintf("private-%d-%d", port, f.tokenCalls[port])
		return canaryapi.PreviewToken{Token: f.tokens[port], Header: "X-Superserve-Preview-Token", Port: port, Access: "private"}, ctx.Err()
	}
	f.client.deletePreviewFn = func(ctx context.Context, id string, port int) error {
		f.previewDeletes++
		f.published[port] = false
		return ctx.Err()
	}
	f.client.previewURLFn = func(id string, port int) string {
		return fmt.Sprintf("https://preview.example/preview/%d", port)
	}
	f.client.execFn = func(ctx context.Context, id, token string, request canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
		if strings.Contains(request.Command, "subprocess.Popen") {
			f.startCalls++
			return canaryapi.ExecResult{}, ctx.Err()
		}
		if strings.Contains(request.Command, "for attempt in range") {
			body, _ := json.Marshal(f.identity)
			return canaryapi.ExecResult{Stdout: string(body)}, ctx.Err()
		}
		f.shortCalls++
		if token != fmt.Sprintf("token-%d", f.activateCalls) || f.status != "active" {
			return canaryapi.ExecResult{}, errors.New("short task used stale credentials or a paused sandbox")
		}
		if f.shortFailure != nil {
			if err := f.shortFailure(f.shortCalls); err != nil {
				return canaryapi.ExecResult{}, err
			}
		}
		value, challenge := soakValuePattern.FindStringSubmatch(request.Command), soakChallengePattern.FindStringSubmatch(request.Command)
		if len(value) != 3 || len(challenge) != 2 {
			return canaryapi.ExecResult{}, fmt.Errorf("short task has no fresh arithmetic/challenge: %s", request.Command)
		}
		left, _ := strconv.Atoi(value[1])
		right, _ := strconv.Atoi(value[2])
		identity := f.identity
		identity.Challenge = challenge[1]
		body, _ := json.Marshal(map[string]any{"value": left + right, "server": identity})
		return canaryapi.ExecResult{Stdout: string(body)}, ctx.Err()
	}
	f.client.deleteSandboxFn = func(ctx context.Context, id string) error {
		f.deleteCalls++
		f.cleanupContextErr = ctx.Err()
		return f.deleteFailure
	}
	f.client.updateSandboxFn = func(context.Context, string, canaryapi.UpdateSandboxRequest) error {
		f.retentionCalls++
		return nil
	}
	f.runner = Runner{
		Config: config.Config{
			Mode: config.ModeSoak, Target: "staging-us-central1", Environment: "staging", Region: "us-central1",
			SoakDuration: 2 * time.Minute, SoakInterval: time.Minute, RunTimeout: 6*time.Hour + 15*time.Minute,
			ResourceTTL: 6*time.Hour + 30*time.Minute, LockTTL: 6*time.Hour + 30*time.Minute,
			CommandTimeout: 45 * time.Second, DeleteTimeout: 45 * time.Second, PreviewTimeout: 20 * time.Second,
			HTTPTimeout: 30 * time.Second, PollInterval: time.Millisecond, PreviewPort: 18080,
			SlackWebhookURL: "https://slack.example/final",
		},
		Client: f.client, Metrics: f.steps, Locker: f.lock,
		Clock: func() time.Time { return f.now },
		Wait: func(ctx context.Context, delay time.Duration) error {
			f.now = f.now.Add(delay)
			return ctx.Err()
		},
		HTTP: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			recorder := httptest.NewRecorder()
			f.serveHTTP(recorder, request)
			return recorder.Result(), nil
		})},
	}
	return f
}

func (f *soakFixture) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Host == "slack.example" {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			f.t.Fatal(err)
		}
		if f.deleteCalls != 1 {
			f.t.Errorf("Slack received intermediate message before cleanup")
		}
		f.slack = append(f.slack, payload.Text)
		f.slackAfterCycles = f.activateCalls
		w.WriteHeader(http.StatusOK)
		return
	}
	port, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(request.URL.Path, "/preview/"), "/"))
	if err != nil {
		f.t.Fatalf("unexpected preview path %q", request.URL.Path)
	}
	f.previewAttempts[port]++
	if f.previewFailure != nil && f.previewFailure(port, f.previewAttempts[port]) {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if !f.published[port] {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if request.Header.Get("X-Superserve-Preview-Token") != f.tokens[port] || f.tokens[port] == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.status != "active" {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	identity := f.identity
	identity.Challenge = request.URL.Query().Get("challenge")
	_ = json.NewEncoder(w).Encode(identity)
}

func (f *soakFixture) wantSummary(parts ...string) {
	f.t.Helper()
	if len(f.slack) != 1 {
		f.t.Fatalf("Slack messages = %d, want one final summary", len(f.slack))
	}
	for _, part := range parts {
		if !strings.Contains(f.slack[0], part) {
			f.t.Errorf("summary missing %q:\n%s", part, f.slack[0])
		}
	}
}

type soakMetrics struct {
	metrics.NoopProvider
	steps         map[string]int
	runScenario   string
	runResult     string
	cleanupResult string
}

func (m *soakMetrics) RecordStep(_ context.Context, _, _, _, scenario, step, result string, _ time.Duration) {
	if m.steps == nil {
		m.steps = make(map[string]int)
	}
	m.steps[step+"/"+result]++
}

func (m *soakMetrics) RecordRun(_ context.Context, _, _, _, scenario, result string, _ time.Duration) {
	m.runScenario, m.runResult = scenario, result
}

func (m *soakMetrics) RecordCleanup(_ context.Context, _, _, _, result string) {
	m.cleanupResult = result
}

func (m *soakMetrics) count(step, result string) int { return m.steps[step+"/"+result] }

type soakLock struct {
	key      string
	released bool
}

func (l *soakLock) Acquire(_ context.Context, key string, _ time.Duration) (lock.Outcome, lock.Lease, error) {
	l.key = key
	return lock.OutcomeAcquired, l, nil
}

func (l *soakLock) Release(context.Context) error {
	l.released = true
	return nil
}
