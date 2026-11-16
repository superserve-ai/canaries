package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/loadrunner"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
	"os"
	"testing"

	"github.com/superserve-ai/canaries/internal/config"
)

func TestLoadrunnerMetricsConfigDefaultsToOTLPWhenEndpointIsSet(t *testing.T) {
	t.Setenv("CANARY_METRICS_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example/v1/metrics")

	cfg := loadrunnerMetricsConfig()
	if cfg.MetricsExporter != config.MetricsExporterOTLP {
		t.Fatalf("MetricsExporter = %q, want %q", cfg.MetricsExporter, config.MetricsExporterOTLP)
	}
	if got := cfg.OTELExporterOTLPMetricsEndpoint; got != "https://collector.example/v1/metrics" {
		t.Fatalf("OTELExporterOTLPMetricsEndpoint = %q", got)
	}
}

func TestLoadrunnerMetricsConfigRespectsExplicitExporter(t *testing.T) {
	t.Setenv("CANARY_METRICS_EXPORTER", string(config.MetricsExporterStdout))
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example/v1/metrics")

	cfg := loadrunnerMetricsConfig()
	if cfg.MetricsExporter != config.MetricsExporterStdout {
		t.Fatalf("MetricsExporter = %q, want %q", cfg.MetricsExporter, config.MetricsExporterStdout)
	}
	if cfg.OTELExporterOTLPMetricsEndpoint != "https://collector.example/v1/metrics" {
		t.Fatalf("OTELExporterOTLPMetricsEndpoint = %q", cfg.OTELExporterOTLPMetricsEndpoint)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// All requests in these tests terminate in the in-memory transport. No
// listener, cloud client, or external endpoint participates in the run.
type cliRoundTripper func(*http.Request) (*http.Response, error)

func (f cliRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type cliEvents struct {
	mu     sync.Mutex
	values []string
}

func (e *cliEvents) add(value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.values = append(e.values, value)
}

func (e *cliEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.values...)
}

type cliMetrics struct {
	metrics.NoopProvider
	mu                 sync.Mutex
	events             *cliEvents
	runs               []string
	deltas             []int64
	executionScenarios []string
	overlaps           int
	overlapScenarios   []string
	ctxErrs            []error
}

func (m *cliMetrics) RecordRun(ctx context.Context, _, _, _, _, result string, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = append(m.runs, result)
	m.ctxErrs = append(m.ctxErrs, ctx.Err())
	m.events.add("run_metric:" + result)
}

func (m *cliMetrics) RecordExecutionDelta(ctx context.Context, _, _, _, scenario string, delta int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deltas = append(m.deltas, delta)
	m.executionScenarios = append(m.executionScenarios, scenario)
	m.ctxErrs = append(m.ctxErrs, ctx.Err())
	if delta > 0 {
		m.events.add("execution_start")
	} else {
		m.events.add("execution_end")
	}
}

func (m *cliMetrics) RecordOverlapSkip(ctx context.Context, _, _, _, scenario string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overlaps++
	m.overlapScenarios = append(m.overlapScenarios, scenario)
	m.ctxErrs = append(m.ctxErrs, ctx.Err())
	m.events.add("overlap_skip")
}

type cliLocker struct {
	events       *cliEvents
	outcome      lock.Outcome
	releaseErr   error
	acquires     int
	releases     int
	closeCalls   int
	releaseCtx   error
	acquiredKeys []string
}

func (l *cliLocker) Acquire(_ context.Context, key string, _ time.Duration) (lock.Outcome, lock.Lease, error) {
	l.acquires++
	l.acquiredKeys = append(l.acquiredKeys, key)
	l.events.add("lease_acquire")
	if l.outcome != lock.OutcomeAcquired {
		return l.outcome, nil, nil
	}
	return l.outcome, cliLease{locker: l}, nil
}

type cliLease struct{ locker *cliLocker }

func (l cliLease) Release(ctx context.Context) error {
	l.locker.releases++
	l.locker.releaseCtx = ctx.Err()
	l.locker.events.add("lease_release")
	return l.locker.releaseErr
}

type cliHarness struct {
	cfg             loadrunner.Config
	events          *cliEvents
	metrics         *cliMetrics
	locker          *cliLocker
	logs            *bytes.Buffer
	createFail      bool
	execFail        bool
	deleteFail      bool
	onExec          func(context.Context)
	summaryErr      error
	summaryStatus   int
	shutdownErr     error
	metricsInitErr  error
	lockerInitErr   error
	sandboxRequests int
	creates         int
	deletes         int
	cleanupContexts []error
	summaries       []string
	summaryContexts []error
	shutdownCalls   int
	unexpected      []string
}

func newCLIHarness(t *testing.T) *cliHarness {
	t.Helper()
	events := &cliEvents{}
	h := &cliHarness{
		cfg: loadrunner.Config{
			Scenario: "create", Environment: "staging", Region: "us-central1", Target: "staging-us-central1",
			APIBaseURL: "https://api-staging.superserve.ai", PreviewDomain: "staging-sandbox.superserve.ai",
			APIKey: "private-api-token", RunID: "cli-test", WorkerID: "worker-0", Template: "test-template",
			Operations: 1, Concurrency: 1, RunTimeout: time.Second, ResourceTTL: time.Hour,
			PollInterval: time.Millisecond, HTTPTimeout: time.Second, CommandTimeout: time.Second,
			ActiveTimeout: time.Second, DeleteTimeout: time.Second,
			SlackWebhookURL: "https://hooks.slack.example/services/private-webhook-token",
			LockBackend:     "file", LockFile: "in-memory-lock", LockTTL: 5 * time.Minute,
		},
		events:        events,
		metrics:       &cliMetrics{events: events},
		locker:        &cliLocker{events: events, outcome: lock.OutcomeAcquired},
		logs:          &bytes.Buffer{},
		summaryStatus: http.StatusOK,
	}
	previousLogger := log.Logger
	log.Logger = zerolog.New(h.logs)
	t.Cleanup(func() { log.Logger = previousLogger })
	return h
}

func cliResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func (h *cliHarness) roundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() == "hooks.slack.example" {
		h.events.add("summary_post")
		h.summaryContexts = append(h.summaryContexts, req.Context().Err())
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			return nil, err
		}
		h.summaries = append(h.summaries, payload.Text)
		if h.summaryErr != nil {
			return nil, h.summaryErr
		}
		return cliResponse(h.summaryStatus, "test webhook response"), nil
	}
	h.sandboxRequests++
	switch {
	case req.URL.Hostname() == "api-staging.superserve.ai" && req.Method == http.MethodPost && req.URL.Path == "/sandboxes":
		h.events.add("create")
		h.creates++
		if h.createFail {
			return cliResponse(http.StatusServiceUnavailable, `{"error":"create unavailable"}`), nil
		}
		return cliResponse(http.StatusCreated, `{"id":"sandbox-1","status":"creating","access_token":"private-access-token"}`), nil
	case req.URL.Hostname() == "api-staging.superserve.ai" && req.Method == http.MethodGet && req.URL.Path == "/sandboxes/sandbox-1":
		h.events.add("get_active")
		return cliResponse(http.StatusOK, `{"id":"sandbox-1","status":"active"}`), nil
	case req.URL.Hostname() == "staging-sandbox.superserve.ai" && req.Method == http.MethodPost && req.URL.Path == "/exec":
		h.events.add("exec")
		if h.onExec != nil {
			h.onExec(req.Context())
		}
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		if h.execFail {
			return cliResponse(http.StatusOK, `{"stdout":"superserve-load-test","exit_code":7}`), nil
		}
		return cliResponse(http.StatusOK, `{"stdout":"superserve-load-test","exit_code":0}`), nil
	case req.URL.Hostname() == "api-staging.superserve.ai" && req.Method == http.MethodDelete && req.URL.Path == "/sandboxes/sandbox-1":
		h.events.add("delete")
		h.deletes++
		h.cleanupContexts = append(h.cleanupContexts, req.Context().Err())
		if h.deleteFail {
			return cliResponse(http.StatusServiceUnavailable, `{"error":"delete unavailable"}`), nil
		}
		return cliResponse(http.StatusNoContent, ""), nil
	default:
		h.unexpected = append(h.unexpected, req.Method+" "+req.URL.Hostname()+req.URL.Path)
		return nil, errors.New("unexpected in-memory test request")
	}
}

func (h *cliHarness) deps() runDependencies {
	return runDependencies{
		HTTP: &http.Client{Transport: cliRoundTripper(h.roundTrip)},
		NewMetrics: func(context.Context, config.Config) (metrics.Provider, func(context.Context) error, error) {
			h.events.add("metrics_init")
			if h.metricsInitErr != nil {
				return nil, nil, h.metricsInitErr
			}
			return h.metrics, func(ctx context.Context) error {
				h.shutdownCalls++
				h.events.add("metrics_shutdown")
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return h.shutdownErr
			}, nil
		},
		NewLocker: func(_ context.Context, cfg lock.Config) (lock.Lock, func(), error) {
			h.events.add("locker_init")
			if h.lockerInitErr != nil {
				return nil, nil, h.lockerInitErr
			}
			if cfg.Backend != h.cfg.LockBackend || cfg.FilePath != h.cfg.LockFile {
				return nil, nil, errors.New("incorrect lock factory configuration")
			}
			return h.locker, func() {
				h.locker.closeCalls++
				h.events.add("locker_close")
			}, nil
		},
	}
}

func (h *cliHarness) assertTerminal(t *testing.T, result string) {
	t.Helper()
	if len(h.summaries) != 1 {
		t.Fatalf("summary posts=%d, want exactly one", len(h.summaries))
	}
	if len(h.unexpected) != 0 {
		t.Fatalf("unexpected requests: %v", h.unexpected)
	}
	if len(h.metrics.runs) != 0 {
		t.Fatalf("load runner emitted overall run metrics before its full job outcome was known: %v", h.metrics.runs)
	}
	var terminal, summaryLogs int
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSON log entry: %v", err)
		}
		switch entry["message"] {
		case "load test completed":
			terminal++
			if entry["result"] != result {
				t.Errorf("terminal result=%v, want %s", entry["result"], result)
			}
		case "load testing summary":
			summaryLogs++
			if entry["summary"] != h.summaries[0] {
				t.Error("posted summary differs from final logged summary")
			}
		}
	}
	if terminal != 1 || summaryLogs != 1 {
		t.Fatalf("terminal logs=%d summary logs=%d, want one each", terminal, summaryLogs)
	}
	for _, private := range []string{"private-api-token", "private-access-token", "private-webhook-token"} {
		if strings.Contains(h.logs.String(), private) || strings.Contains(h.summaries[0], private) {
			t.Error("terminal logs or public summary exposed a credential")
		}
	}
}

func assertEventOrder(t *testing.T, events []string, required ...string) {
	t.Helper()
	next := 0
	for _, event := range events {
		if next < len(required) && event == required[next] {
			next++
		}
	}
	if next != len(required) {
		t.Fatalf("event order %v does not contain %v", events, required)
	}
}

func TestRunConfiguredPostsOneFinalSummaryAfterCleanupAndLeaseRelease(t *testing.T) {
	for _, mode := range []string{"success", "create failure", "exec failure", "cleanup failure", "lease release failure"} {
		t.Run(mode, func(t *testing.T) {
			h := newCLIHarness(t)
			h.createFail = mode == "create failure"
			h.execFail = mode == "exec failure"
			h.deleteFail = mode == "cleanup failure"
			if mode == "lease release failure" {
				h.locker.releaseErr = errors.New("release unavailable")
			}
			err := runConfigured(context.Background(), h.cfg, h.deps())
			wantResult := "success"
			if mode != "success" {
				wantResult = "failure"
			}
			if (err != nil) != (wantResult == "failure") {
				t.Fatalf("runConfigured returned %v for %s", err, mode)
			}
			h.assertTerminal(t, wantResult)
			if h.creates != 1 || h.locker.acquires != 1 || h.locker.releases != 1 || h.locker.closeCalls != 1 || h.shutdownCalls != 1 {
				t.Fatalf("creates=%d acquire=%d release=%d close=%d shutdown=%d", h.creates, h.locker.acquires, h.locker.releases, h.locker.closeCalls, h.shutdownCalls)
			}
			if !reflect.DeepEqual(h.locker.acquiredKeys, []string{h.cfg.Target + "-loadtest"}) || !reflect.DeepEqual(h.metrics.deltas, []int64{1, -1}) || h.metrics.overlaps != 0 || !reflect.DeepEqual(h.metrics.executionScenarios, []string{"loadtest-create", "loadtest-create"}) {
				t.Fatalf("unexpected coordination or terminal telemetry: keys=%v deltas=%v runs=%v overlap=%d scenarios=%v", h.locker.acquiredKeys, h.metrics.deltas, h.metrics.runs, h.metrics.overlaps, h.metrics.executionScenarios)
			}
			if h.createFail {
				if h.deletes != 0 {
					t.Fatal("failed create without ID should not issue deletion")
				}
				assertEventOrder(t, h.events.snapshot(), "create", "execution_end", "lease_release", "locker_close", "metrics_shutdown", "summary_post")
			} else {
				if h.deletes != 1 {
					t.Fatal("created sandbox did not receive exactly one cleanup attempt")
				}
				assertEventOrder(t, h.events.snapshot(), "delete", "execution_end", "lease_release", "locker_close", "metrics_shutdown", "summary_post")
			}
			if wantResult == "success" && !strings.Contains(h.summaries[0], "Sandboxes: 1 total, 1 started, 1 succeeded, 0 failed, 0 unstarted") {
				t.Fatal("successful summary has incorrect final counts")
			}
			if wantResult == "failure" && !strings.Contains(h.summaries[0], "Outcome: failed") {
				t.Fatal("final summary omitted run failure")
			}
		})
	}
}

func TestRunConfiguredGuardBlockedPostsFailureWithoutSandboxRequests(t *testing.T) {
	h := newCLIHarness(t)
	h.cfg.Operations = 100
	h.locker.outcome = lock.OutcomeAlreadyRunning
	err := runConfigured(context.Background(), h.cfg, h.deps())
	if !errors.Is(err, loadrunner.ErrGuardBlocked) {
		t.Fatalf("runConfigured error=%v, want blocked guard", err)
	}
	h.assertTerminal(t, "failure")
	if h.sandboxRequests != 0 || h.locker.releases != 0 || h.locker.closeCalls != 1 || h.metrics.overlaps != 1 || !reflect.DeepEqual(h.metrics.overlapScenarios, []string{"loadtest-create"}) || len(h.metrics.deltas) != 0 {
		t.Fatalf("blocked run performed workload or incorrect telemetry: requests=%d releases=%d closes=%d overlaps=%d deltas=%v runs=%v", h.sandboxRequests, h.locker.releases, h.locker.closeCalls, h.metrics.overlaps, h.metrics.deltas, h.metrics.runs)
	}
	if !strings.Contains(h.summaries[0], "Sandboxes: 100 total, 0 started, 0 succeeded, 0 failed, 100 unstarted") || !strings.Contains(h.summaries[0], "Outcome: failed") {
		t.Fatal("blocked summary omitted failure or unstarted count")
	}
	assertEventOrder(t, h.events.snapshot(), "lease_acquire", "overlap_skip", "locker_close", "metrics_shutdown", "summary_post")
}

func TestRunConfiguredCanceledWorkStillCleansAndReports(t *testing.T) {
	h := newCLIHarness(t)
	h.cfg.Operations = 100
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.onExec = func(execCtx context.Context) {
		cancel()
		<-execCtx.Done()
	}
	err := runConfigured(ctx, h.cfg, h.deps())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runConfigured error=%v, want cancellation", err)
	}
	h.assertTerminal(t, "failure")
	if h.creates != 1 || h.deletes != 1 || h.locker.releases != 1 || h.locker.releaseCtx != nil || !reflect.DeepEqual(h.cleanupContexts, []error{nil}) || !reflect.DeepEqual(h.summaryContexts, []error{nil}) {
		t.Fatalf("cancellation escaped cleanup/reporting contexts: create=%d delete=%d release=%d releaseCtx=%v cleanup=%v summary=%v", h.creates, h.deletes, h.locker.releases, h.locker.releaseCtx, h.cleanupContexts, h.summaryContexts)
	}
	if !strings.Contains(h.summaries[0], "Sandboxes: 100 total, 1 started, 0 succeeded, 1 failed, 99 unstarted") || !strings.Contains(h.summaries[0], "Outcome: canceled") {
		t.Fatal("canceled summary omitted final counts or cancellation outcome")
	}
	if !reflect.DeepEqual(h.metrics.deltas, []int64{1, -1}) || h.metrics.overlaps != 0 {
		t.Fatal("canceled run did not balance execution telemetry")
	}
	for _, ctxErr := range h.metrics.ctxErrs {
		if ctxErr != nil {
			t.Fatal("terminal telemetry inherited a canceled context")
		}
	}
	assertEventOrder(t, h.events.snapshot(), "delete", "execution_end", "lease_release", "locker_close", "metrics_shutdown", "summary_post")
}

func TestRunConfiguredSummaryDeliveryFailureIsGenericAndAlertable(t *testing.T) {
	for _, mode := range []string{"transport", "HTTP status"} {
		t.Run(mode, func(t *testing.T) {
			h := newCLIHarness(t)
			if mode == "transport" {
				h.summaryErr = errors.New("cannot deliver " + h.cfg.SlackWebhookURL)
			} else {
				h.summaryStatus = http.StatusServiceUnavailable
			}
			err := runConfigured(context.Background(), h.cfg, h.deps())
			if err == nil || !strings.Contains(err.Error(), "load test summary delivery failed") {
				t.Fatalf("runConfigured error=%v, want summary delivery failure", err)
			}
			if strings.Contains(err.Error(), "private-webhook-token") || strings.Contains(err.Error(), h.cfg.SlackWebhookURL) {
				t.Fatal("summary delivery error exposed webhook URL")
			}
			h.assertTerminal(t, "failure")
			if !reflect.DeepEqual(h.metrics.deltas, []int64{1, -1}) || h.shutdownCalls != 1 || h.deletes != 1 || h.locker.releases != 1 {
				t.Fatal("summary delivery failure was not reflected in terminal telemetry or skipped finalization")
			}
			assertEventOrder(t, h.events.snapshot(), "delete", "lease_release", "locker_close", "metrics_shutdown", "summary_post")
		})
	}
}

func TestRunConfiguredMetricsShutdownFailureFailsJob(t *testing.T) {
	h := newCLIHarness(t)
	h.shutdownErr = errors.New("private-metrics-exporter-error")
	err := runConfigured(context.Background(), h.cfg, h.deps())
	if err == nil || err.Error() != "load test metrics export failed" {
		t.Fatalf("unexpected metrics shutdown result: %v", err)
	}
	h.assertTerminal(t, "failure")
	if h.shutdownCalls != 1 || h.deletes != 1 || h.locker.releases != 1 || !reflect.DeepEqual(h.metrics.deltas, []int64{1, -1}) {
		t.Fatal("metrics export failure skipped or duplicated finalization")
	}
	if !strings.Contains(h.summaries[0], "Outcome: failed") || strings.Contains(h.summaries[0], "Outcome: succeeded") {
		t.Fatal("metrics export failure was omitted from the single final summary")
	}
	if !strings.Contains(h.summaries[0], "Sandboxes: 1 total, 1 started, 1 succeeded, 0 failed, 0 unstarted") {
		t.Fatal("metrics export failure incorrectly changed successful lifecycle counts")
	}
	if strings.Contains(h.summaries[0], "private-metrics-exporter-error") || strings.Contains(h.logs.String(), "private-metrics-exporter-error") {
		t.Fatal("metrics export failure exposed the private exporter error")
	}
	assertEventOrder(t, h.events.snapshot(), "delete", "execution_end", "lease_release", "locker_close", "metrics_shutdown", "summary_post")
}

func cliLogEntries(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSON log entry: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func cliWarnings(entries []map[string]any) []map[string]any {
	var warnings []map[string]any
	for _, entry := range entries {
		if entry["level"] == "warn" {
			warnings = append(warnings, entry)
		}
	}
	return warnings
}

func TestRunConfiguredSamplingPeriodRejectionDoesNotFailJob(t *testing.T) {
	h := newCLIHarness(t)
	h.shutdownErr = errors.New(`failed to upload metrics: 400 Bad Request {"code":400,"message":"points were written more frequently than the maximum sampling period","status":"FAILED_PRECONDITION"}`)
	if err := runConfigured(context.Background(), h.cfg, h.deps()); err != nil {
		t.Fatalf("sampling period rejection failed the run: %v", err)
	}
	h.assertTerminal(t, "success")
	if h.shutdownCalls != 1 || h.deletes != 1 || h.locker.releases != 1 || !reflect.DeepEqual(h.metrics.deltas, []int64{1, -1}) {
		t.Fatal("sampling period rejection skipped or duplicated finalization")
	}
	if !strings.Contains(h.summaries[0], "Outcome: succeeded") || !strings.Contains(h.summaries[0], "Sandboxes: 1 total, 1 started, 1 succeeded, 0 failed, 0 unstarted") {
		t.Fatal("sampling period rejection changed the successful final summary")
	}
	entries := cliLogEntries(t, h.logs.String())
	warnings := cliWarnings(entries)
	if len(warnings) != 1 {
		t.Fatalf("warnings=%v, want exactly one", warnings)
	}
	if warnings[0]["message"] != "load test metrics export skipped by sampling period" || warnings[0]["run_id"] != h.cfg.RunID || warnings[0]["exporter_status"] != float64(400) {
		t.Fatalf("unexpected sampling period warning: %v", warnings[0])
	}
	for _, entry := range entries {
		if _, ok := entry["error"]; ok {
			t.Fatalf("sampling period rejection logged an alertable error field: %v", entry)
		}
	}
	for _, raw := range []string{"failed to upload", "FAILED_PRECONDITION", "maximum sampling period"} {
		if strings.Contains(h.logs.String(), raw) || strings.Contains(h.summaries[0], raw) {
			t.Fatalf("sampling period rejection exposed raw exporter text %q", raw)
		}
	}
	assertEventOrder(t, h.events.snapshot(), "delete", "execution_end", "lease_release", "locker_close", "metrics_shutdown", "summary_post")
}

func TestRunConfiguredMetricsShutdownFailureLogsOnlyExporterStatus(t *testing.T) {
	h := newCLIHarness(t)
	h.shutdownErr = errors.New("failed to upload metrics: failed to send metrics to https://collector.example/v1/metrics?token=private-collector-token: 403 Forbidden (body: permission denied)")
	err := runConfigured(context.Background(), h.cfg, h.deps())
	if err == nil || err.Error() != "load test metrics export failed" {
		t.Fatalf("unexpected metrics shutdown result: %v", err)
	}
	h.assertTerminal(t, "failure")
	if !strings.Contains(h.summaries[0], "Outcome: failed") {
		t.Fatal("metrics export failure was omitted from the single final summary")
	}
	warnings := cliWarnings(cliLogEntries(t, h.logs.String()))
	if len(warnings) != 1 {
		t.Fatalf("warnings=%v, want exactly one", warnings)
	}
	if warnings[0]["message"] != "load test metrics export failed" || warnings[0]["run_id"] != h.cfg.RunID || warnings[0]["exporter_status"] != float64(403) {
		t.Fatalf("unexpected metrics export warning: %v", warnings[0])
	}
	if _, ok := warnings[0]["error"]; ok {
		t.Fatal("metrics export warning carried the raw exporter error")
	}
	for _, raw := range []string{"collector.example", "private-collector-token", "permission denied"} {
		if strings.Contains(h.logs.String(), raw) || strings.Contains(h.summaries[0], raw) || strings.Contains(err.Error(), raw) {
			t.Fatalf("metrics export failure exposed raw exporter text %q", raw)
		}
	}
}

func TestRunConfiguredInitializationFailureStillPostsUnstartedSummary(t *testing.T) {
	for _, mode := range []string{"metrics", "locker"} {
		t.Run(mode, func(t *testing.T) {
			h := newCLIHarness(t)
			h.cfg.Operations = 100
			if mode == "metrics" {
				h.metricsInitErr = errors.New("metrics unavailable")
			} else {
				h.lockerInitErr = errors.New("coordination unavailable")
			}
			if err := runConfigured(context.Background(), h.cfg, h.deps()); err == nil {
				t.Fatal("initialization failure returned success")
			}
			h.assertTerminal(t, "failure")
			if h.sandboxRequests != 0 || h.locker.acquires != 0 || len(h.metrics.deltas) != 0 || !strings.Contains(h.summaries[0], "Sandboxes: 100 total, 0 started, 0 succeeded, 0 failed, 100 unstarted") {
				t.Fatal("initialization failure started workload or omitted unstarted count")
			}
			if mode == "locker" {
				if h.shutdownCalls != 1 {
					t.Fatal("coordination initialization failure did not finalize initialized metrics")
				}
				assertEventOrder(t, h.events.snapshot(), "metrics_init", "locker_init", "metrics_shutdown", "summary_post")
			} else if h.shutdownCalls != 0 {
				t.Fatal("metrics initialization failure shut down an uninitialized provider")
			}
		})
	}
}
