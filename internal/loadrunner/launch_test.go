package loadrunner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/metrics"
)

func setLaunchEnv(t *testing.T) {
	t.Helper()
	setRequiredEnv(t)
	for _, name := range []string{
		"LOAD_TEST_OPERATIONS", "LOAD_TEST_CONCURRENCY", "LOAD_TEST_WORKER_ID", "LOAD_TEST_SANDBOX_TEMPLATE",
		"LOAD_TEST_LAUNCH_STRATEGY", "LOAD_TEST_LAUNCH_RATE", "LOAD_TEST_LAUNCH_HOLD", "LOAD_TEST_LAUNCH_CEILING",
		"LOAD_TEST_RAMP_START_RATE", "LOAD_TEST_RAMP_STEP_RATE", "LOAD_TEST_RAMP_STEP_DURATION",
		"LOAD_TEST_API_RPS", "LOAD_TEST_TEARDOWN_TIMEOUT", "LOAD_TEST_TEARDOWN_CONCURRENCY",
		"LOAD_TEST_TASK_INDEX", "LOAD_TEST_TASK_COUNT", "CLOUD_RUN_TASK_INDEX", "CLOUD_RUN_TASK_COUNT",
		"LOAD_TEST_RUN_TIMEOUT", "LOAD_TEST_RESOURCE_TTL",
		"LOAD_TEST_GUARD_MODE", "LOAD_TEST_GUARD_INTERVAL", "LOAD_TEST_GUARD_BACKOFF_PAUSE", "LOAD_TEST_GUARD_MAX_BACKOFFS",
		"LOAD_TEST_GUARD_SOURCE_TIMEOUT", "LOAD_TEST_GUARD_GMP_PROJECT", "LOAD_TEST_GUARD_GMP_ENVIRONMENT",
		"LOAD_TEST_GUARD_CREATE_WINDOW", "LOAD_TEST_GUARD_CREATE_MIN_SAMPLES", "LOAD_TEST_GUARD_HOST_IDS",
		"LOAD_TEST_GUARD_FREE_MEMORY_PCT_BACKOFF", "LOAD_TEST_GUARD_FREE_MEMORY_PCT_ABORT",
		"LOAD_TEST_PRESSURE_SUPABASE_URL", "LOAD_TEST_PRESSURE_SUPABASE_KEY",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("LOAD_TEST_SCENARIO", ScenarioLaunch)
}

func TestLoadLaunchDefaults(t *testing.T) {
	setLaunchEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	if cfg.Template != defaultLaunchTemplate || cfg.Operations != 100 || cfg.Concurrency != 100 || cfg.Launch.Ceiling != defaultLaunchCeiling {
		t.Fatalf("unexpected launch defaults: template=%q operations=%d concurrency=%d ceiling=%d", cfg.Template, cfg.Operations, cfg.Concurrency, cfg.Launch.Ceiling)
	}
	if cfg.Launch.Strategy != "burst" || cfg.Launch.APIRate != defaultLaunchAPIRate || cfg.Launch.TaskCount != 1 || cfg.WorkerID != "worker-0" {
		t.Fatalf("unexpected launch config: %+v worker=%q", cfg.Launch, cfg.WorkerID)
	}
	if cfg.Pressure.Mode != PressureModeEnforce || len(cfg.Pressure.Rules) != len(defaultPressureRules()) {
		t.Fatalf("unexpected guard config: %+v", cfg.Pressure)
	}
	if cfg.LockTTL != time.Hour || cfg.CleanupBudget() != cfg.DeleteTimeout+cfg.Launch.TeardownTimeout {
		t.Fatalf("lock TTL %s or cleanup budget %s does not cover the teardown", cfg.LockTTL, cfg.CleanupBudget())
	}
}

func TestLoadLaunchRequiresExplicitCeiling(t *testing.T) {
	setLaunchEnv(t)
	t.Setenv("LOAD_TEST_OPERATIONS", "101")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LOAD_TEST_LAUNCH_CEILING") {
		t.Fatalf("expected the default ceiling to reject 101 launches, got %v", err)
	}
	t.Setenv("LOAD_TEST_LAUNCH_CEILING", "10000")
	t.Setenv("LOAD_TEST_OPERATIONS", "10000")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load rejected a 10K launch under an explicit ceiling: %v", err)
	}
	if cfg.Concurrency != defaultLaunchConcurrency {
		t.Fatalf("Concurrency = %d, want %d", cfg.Concurrency, defaultLaunchConcurrency)
	}
}

func TestLoadLaunchFanOut(t *testing.T) {
	setLaunchEnv(t)
	t.Setenv("LOAD_TEST_LAUNCH_CEILING", "100000")
	t.Setenv("LOAD_TEST_OPERATIONS", "20000")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LOAD_TEST_TASK_COUNT") {
		t.Fatalf("expected a single task to reject 20K launches, got %v", err)
	}
	t.Setenv("CLOUD_RUN_TASK_COUNT", "2")
	t.Setenv("CLOUD_RUN_TASK_INDEX", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load rejected a two-task fan-out: %v", err)
	}
	if cfg.WorkerID != "task-1" || cfg.Launch.TaskIndex != 1 || cfg.Launch.TaskOperations(cfg.Operations) != 10000 {
		t.Fatalf("unexpected fan-out task: worker=%q launch=%+v", cfg.WorkerID, cfg.Launch)
	}
	t.Setenv("CANARY_LOCK_BACKEND", "file")
	t.Setenv("CANARY_LOCK_FILE", t.TempDir()+"/lock")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CANARY_LOCK_BACKEND=none") {
		t.Fatalf("expected fan-out with a shared lock to be rejected, got %v", err)
	}
}

func TestLoadLaunchProductionRequirements(t *testing.T) {
	setLaunchEnv(t)
	setProductionEnv(t, "us-east4", "https://api.superserve.ai", "use-sandbox.superserve.ai")
	t.Setenv("LOAD_TEST_SCENARIO", ScenarioLaunch)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "GCS coordination") {
		t.Fatalf("expected production launch without GCS coordination to be rejected, got %v", err)
	}
	t.Setenv("CANARY_LOCK_BACKEND", "gcs")
	t.Setenv("LOCK_BUCKET", "bucket")
	t.Setenv("LOAD_TEST_GLOBAL_LOCK", "true")
	t.Setenv("LOAD_TEST_BENCHMARK_GUARD", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "explicit LOAD_TEST_LAUNCH_CEILING") {
		t.Fatalf("expected production launch without an explicit ceiling to be rejected, got %v", err)
	}
	t.Setenv("LOAD_TEST_LAUNCH_CEILING", "100")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "host pressure source") {
		t.Fatalf("expected production launch without a host source to be rejected, got %v", err)
	}
	t.Setenv("LOAD_TEST_GUARD_GMP_PROJECT", "example-project")
	for _, signal := range []string{"RESTORE_P99_SECONDS", "RESTORE_ERROR_RATIO", "DB_POOL_UTILIZATION"} {
		t.Setenv("LOAD_TEST_GUARD_"+signal+"_BACKOFF", "0")
		t.Setenv("LOAD_TEST_GUARD_"+signal+"_ABORT", "0")
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "host pressure source") {
		t.Fatalf("expected a GMP source with every guarded query disabled to be rejected, got %v", err)
	}
	for _, signal := range []string{"RESTORE_P99_SECONDS", "RESTORE_ERROR_RATIO", "DB_POOL_UTILIZATION"} {
		t.Setenv("LOAD_TEST_GUARD_"+signal+"_BACKOFF", "")
		t.Setenv("LOAD_TEST_GUARD_"+signal+"_ABORT", "")
	}
	t.Setenv("LOAD_TEST_GUARD_MODE", PressureModeObserve)
	if _, err := Load(); err == nil {
		t.Fatal("expected production launch in observe mode to be rejected")
	}
	t.Setenv("LOAD_TEST_GUARD_MODE", "")
	if _, err := Load(); err != nil {
		t.Fatalf("Load rejected a fully guarded production launch config: %v", err)
	}
	t.Setenv("LOAD_TEST_PRODUCTION_OPT_IN", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected production launch without opt-in to be rejected")
	}
}

func TestLoadLaunchGuardOverridesAndValidation(t *testing.T) {
	setLaunchEnv(t)
	t.Setenv("LOAD_TEST_GUARD_FREE_MEMORY_PCT_BACKOFF", "20")
	t.Setenv("LOAD_TEST_GUARD_FREE_MEMORY_PCT_ABORT", "8")
	t.Setenv("LOAD_TEST_GUARD_HOST_IDS", "host-a, host-b")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	var found bool
	for _, rule := range cfg.Pressure.Rules {
		if rule.Signal == "free_memory_pct" {
			found = rule.Backoff == 20 && rule.Abort == 8
		}
	}
	if !found || strings.Join(cfg.Pressure.HostIDs, ",") != "host-a,host-b" {
		t.Fatalf("guard overrides not applied: %+v", cfg.Pressure)
	}

	tests := []struct {
		name  string
		env   map[string]string
		error string
	}{
		{name: "mode", env: map[string]string{"LOAD_TEST_GUARD_MODE": "loud"}, error: "LOAD_TEST_GUARD_MODE"},
		{name: "abort less severe", env: map[string]string{"LOAD_TEST_GUARD_FREE_MEMORY_PCT_ABORT": "30"}, error: "at least as severe"},
		{name: "negative threshold", env: map[string]string{"LOAD_TEST_GUARD_FREE_MEMORY_PCT_BACKOFF": "-1"}, error: "non-negative"},
		{name: "supabase without key", env: map[string]string{"LOAD_TEST_PRESSURE_SUPABASE_URL": "https://example.supabase.co"}, error: "set together"},
		{name: "supabase http", env: map[string]string{"LOAD_TEST_PRESSURE_SUPABASE_URL": "http://example.supabase.co", "LOAD_TEST_PRESSURE_SUPABASE_KEY": "key"}, error: "HTTPS"},
		{name: "host id", env: map[string]string{"LOAD_TEST_GUARD_HOST_IDS": "host a"}, error: "LOAD_TEST_GUARD_HOST_IDS"},
		{name: "gmp environment", env: map[string]string{"LOAD_TEST_GUARD_GMP_PROJECT": "example-project", "LOAD_TEST_GUARD_GMP_ENVIRONMENT": `staging"} or vector(1) #`}, error: "LOAD_TEST_GUARD_GMP_ENVIRONMENT"},
		{name: "interval", env: map[string]string{"LOAD_TEST_GUARD_INTERVAL": "10ms"}, error: "safety bounds"},
		{name: "strategy", env: map[string]string{"LOAD_TEST_LAUNCH_STRATEGY": "spike"}, error: "burst or ramp"},
		{name: "ramp without rate", env: map[string]string{"LOAD_TEST_LAUNCH_STRATEGY": "ramp"}, error: "ramp launches"},
		{name: "task index", env: map[string]string{"LOAD_TEST_TASK_COUNT": "2", "LOAD_TEST_TASK_INDEX": "2"}, error: "LOAD_TEST_TASK_INDEX"},
		{name: "concurrency above share", env: map[string]string{"LOAD_TEST_CONCURRENCY": "101"}, error: "LOAD_TEST_CONCURRENCY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setLaunchEnv(t)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.error) {
				t.Fatalf("Load error = %v, want %q", err, tt.error)
			}
			if err != nil && strings.Contains(err.Error(), "test-key") {
				t.Fatalf("config error leaked the API key: %v", err)
			}
		})
	}
}

func TestLaunchRateRamp(t *testing.T) {
	r := Runner{Config: Config{Launch: LaunchConfig{Strategy: "ramp", Rate: 4, RampStartRate: 1, RampStepRate: 2, RampStepDuration: 10 * time.Second}}}
	for elapsed, want := range map[time.Duration]float64{0: 1, 9 * time.Second: 1, 10 * time.Second: 3, 25 * time.Second: 4, time.Hour: 4} {
		if got := r.launchRate(elapsed); got != want {
			t.Fatalf("launchRate(%s) = %v, want %v", elapsed, got, want)
		}
	}
	r.Config.Launch.Strategy = "burst"
	if got := r.launchRate(time.Hour); got != 4 {
		t.Fatalf("burst launchRate = %v, want 4", got)
	}
}

func TestTaskOperationsPartitionsEveryOperation(t *testing.T) {
	for _, total := range []int{1, 2, 10, 10001} {
		for _, count := range []int{1, 3, 7} {
			sum := 0
			for index := range count {
				sum += LaunchConfig{TaskIndex: index, TaskCount: count}.TaskOperations(total)
			}
			if sum != total {
				t.Fatalf("total=%d count=%d: tasks own %d operations", total, count, sum)
			}
		}
	}
}

func TestClassifyCreateFailure(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	status := func(code int, body string) error {
		return &canaryapi.HTTPStatusError{Method: "POST", Path: "/sandboxes", StatusCode: code, Body: body}
	}
	tests := []struct {
		name     string
		ctx      context.Context
		err      error
		allFull  bool
		expected string
	}{
		{"team quota", context.Background(), status(429, `{"error":{"code":"too_many_sandboxes","message":"quota"}}`), false, FailureTeamQuota},
		{"rate limit", context.Background(), status(429, `{"error":{"code":"rate_limited","message":"slow down"}}`), false, FailureRateLimited},
		{"no hosts", context.Background(), status(503, `{"error":{"code":"unavailable","message":"No hosts available"}}`), false, FailureHostCap},
		{"no slots", context.Background(), status(500, `{"error":{"code":"internal","message":"vmd: no available network slots"}}`), false, FailureHostCap},
		{"all hosts full", context.Background(), status(500, `{"error":{"code":"internal","message":"boom"}}`), true, FailureHostCap},
		{"server", context.Background(), status(502, `bad gateway`), false, FailureServerError},
		{"timeout", context.Background(), status(408, ``), false, FailureTransport},
		{"rejected", context.Background(), status(400, `{"error":{"code":"bad_request"}}`), false, FailureRejected},
		{"not sent", context.Background(), errRequestNotSent, false, FailureCanceled},
		{"canceled", canceled, errors.New("context canceled"), false, FailureCanceled},
		{"transport", context.Background(), errors.New("connection reset"), false, FailureTransport},
	}
	for _, tt := range tests {
		if got := classifyCreateFailure(tt.ctx, tt.err, tt.allFull); got != tt.expected {
			t.Errorf("%s: class = %q, want %q", tt.name, got, tt.expected)
		}
	}
}

// fakeLaunchClient keeps a server-side view of live sandboxes so tests can
// verify the teardown sweep.
type fakeLaunchClient struct {
	*fakeLoadClient
	liveMu sync.Mutex
	live   map[string]canaryapi.Sandbox
	// lostCreates makes this many creates succeed on the server but fail
	// in transit, so only the metadata sweep can find them.
	lostCreates atomic.Int64
	// lateCreates makes this many creates fail in transit and land on the
	// server lateBy afterwards, or when landPending is called if lateBy is
	// negative.
	lateCreates atomic.Int64
	lateBy      time.Duration
	pending     []canaryapi.Sandbox
	// afterList runs after a list has taken its snapshot.
	afterList func(call int)
	lists     int
	creates   atomic.Int64
	names     []string
}

func newFakeLaunchClient() *fakeLaunchClient {
	return &fakeLaunchClient{fakeLoadClient: &fakeLoadClient{}, live: make(map[string]canaryapi.Sandbox)}
}

func (f *fakeLaunchClient) CreateSandbox(_ context.Context, req canaryapi.CreateSandboxRequest) (canaryapi.Sandbox, error) {
	f.creates.Add(1)
	time.Sleep(time.Millisecond)
	sb := canaryapi.Sandbox{ID: req.Name, Name: req.Name, Status: "active", AccessToken: "token", Metadata: req.Metadata}
	if f.lateCreates.Add(-1) >= 0 {
		if f.lateBy < 0 {
			f.liveMu.Lock()
			f.pending = append(f.pending, sb)
			f.liveMu.Unlock()
			return canaryapi.Sandbox{}, errors.New("connection reset by peer")
		}
		time.AfterFunc(f.lateBy, func() {
			f.liveMu.Lock()
			f.live[sb.ID] = sb
			f.liveMu.Unlock()
		})
		return canaryapi.Sandbox{}, errors.New("connection reset by peer")
	}
	f.liveMu.Lock()
	f.live[sb.ID] = sb
	f.names = append(f.names, req.Name)
	f.liveMu.Unlock()
	if f.lostCreates.Add(-1) >= 0 {
		return canaryapi.Sandbox{}, errors.New("connection reset by peer")
	}
	return sb, nil
}

func (f *fakeLaunchClient) Exec(context.Context, string, string, canaryapi.ExecRequest) (canaryapi.ExecResult, error) {
	return canaryapi.ExecResult{Stdout: "superserve-load-test"}, nil
}

func (f *fakeLaunchClient) DeleteSandbox(_ context.Context, id string) error {
	f.liveMu.Lock()
	defer f.liveMu.Unlock()
	if _, ok := f.live[id]; !ok {
		return canaryapi.ErrNotFound
	}
	delete(f.live, id)
	return nil
}

func (f *fakeLaunchClient) landPending() {
	f.liveMu.Lock()
	defer f.liveMu.Unlock()
	for _, sb := range f.pending {
		f.live[sb.ID] = sb
	}
	f.pending = nil
}

func (f *fakeLaunchClient) ListSandboxes(_ context.Context, query map[string]string) ([]canaryapi.Sandbox, error) {
	out := f.listSnapshot(query)
	f.liveMu.Lock()
	f.lists++
	call := f.lists
	f.liveMu.Unlock()
	if f.afterList != nil {
		f.afterList(call)
	}
	return out, nil
}

func (f *fakeLaunchClient) listSnapshot(query map[string]string) []canaryapi.Sandbox {
	f.liveMu.Lock()
	defer f.liveMu.Unlock()
	var out []canaryapi.Sandbox
	for _, sb := range f.live {
		match := true
		for key, value := range query {
			if name, ok := strings.CutPrefix(key, "metadata."); ok && sb.Metadata[name] != value {
				match = false
			}
		}
		if match {
			out = append(out, sb)
		}
	}
	return out
}

func (f *fakeLaunchClient) ListTemplates(context.Context, map[string]string) ([]canaryapi.Template, error) {
	return []canaryapi.Template{{ID: "tpl-1", Name: defaultLaunchTemplate, VCPU: 1, MemoryMiB: 512, DiskMiB: 4096, SizeBytes: 300 << 20}}, nil
}

func (f *fakeLaunchClient) liveCount() int {
	f.liveMu.Lock()
	defer f.liveMu.Unlock()
	return len(f.live)
}

func testLaunchConfig(operations int) Config {
	cfg := testRunnerConfig()
	cfg.Scenario = ScenarioLaunch
	cfg.Template = defaultLaunchTemplate
	cfg.Operations = operations
	cfg.Concurrency = 4
	cfg.RunTimeout = 10 * time.Second
	cfg.HTTPTimeout = time.Second
	cfg.Launch = LaunchConfig{Strategy: "burst", APIRate: 10_000, TeardownTimeout: 5 * time.Second, TeardownConcurrency: 4, TaskCount: 1}
	return cfg
}

func launchRunner(cfg Config, client lifecycle.Client) Runner {
	return Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: metrics.NoopProvider{}}}
}

func TestRunLaunchCreatesVerifiesAndCleansUp(t *testing.T) {
	client := newFakeLaunchClient()
	cfg := testLaunchConfig(20)
	summary, err := launchRunner(cfg, client).RunLaunch(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunLaunch returned %v", err)
	}
	if summary.Dispatched != 20 || summary.Created != 20 || summary.Ready != 20 || summary.Failed != 0 || summary.Unstarted != 0 {
		t.Fatalf("unexpected counts: %+v", summary)
	}
	if len(summary.TimeToFirstCommand) != 20 || summary.PeakInFlight > int64(cfg.Concurrency) || summary.PeakInFlight < 1 {
		t.Fatalf("unexpected latency samples or in-flight peak: tti=%d peak=%d", len(summary.TimeToFirstCommand), summary.PeakInFlight)
	}
	if !summary.Template.Known || summary.Template.VCPU != 1 || summary.Template.MemoryMiB != 512 {
		t.Fatalf("template size not recorded: %+v", summary.Template)
	}
	if !summary.Cleanup.Verified || summary.Cleanup.Deleted != 20 || client.liveCount() != 0 {
		t.Fatalf("cleanup not verified: %+v live=%d", summary.Cleanup, client.liveCount())
	}
	report := summary.Report(cfg, err)
	for _, want := range []string{"*Mass Launch Results*", "Outcome: succeeded", "1 vCPU | 512 MiB memory", "Create -> 1st command", "Failures by class: none", "Bottleneck:", "Host pressure guard: off", "verified=true", "Correlate:"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
	totals := summary.Totals()
	if totals.Started != 20 || totals.Completed != 20 || totals.Failed != 0 || totals.CleanupFailures != 0 {
		t.Fatalf("unexpected totals: %+v", totals)
	}
}

func TestRunLaunchSweepsLostCreates(t *testing.T) {
	client := newFakeLaunchClient()
	client.lostCreates.Store(2)
	cfg := testLaunchConfig(10)
	summary, err := launchRunner(cfg, client).RunLaunch(context.Background(), nil)
	if err == nil {
		t.Fatal("expected failed creates to fail the run")
	}
	if summary.Failed != 2 || summary.UncertainCreates != 2 || summary.FailureClasses[FailureTransport] != 2 || summary.Ready != 8 {
		t.Fatalf("unexpected failure accounting: %+v", summary)
	}
	if !summary.Cleanup.Verified || summary.Cleanup.Swept != 2 || client.liveCount() != 0 {
		t.Fatalf("lost creates not swept: %+v live=%d", summary.Cleanup, client.liveCount())
	}
	report := summary.Report(cfg, err)
	if !strings.Contains(report, "transport=2") || !strings.Contains(report, "all 10: 2") {
		t.Fatalf("report missing failure classes or scale:\n%s", report)
	}
}

func TestRunLaunchWaitsForUncertainCreatesToSettle(t *testing.T) {
	client := newFakeLaunchClient()
	client.lateCreates.Store(1)
	client.lateBy = 300 * time.Millisecond
	cfg := testLaunchConfig(4)
	summary, _ := launchRunner(cfg, client).RunLaunch(context.Background(), nil)
	if summary.UncertainCreates != 1 {
		t.Fatalf("UncertainCreates = %d, want 1", summary.UncertainCreates)
	}
	if !summary.Cleanup.Verified || summary.Cleanup.Swept != 1 || client.liveCount() != 0 {
		t.Fatalf("late create not swept before cleanup was verified: %+v live=%d", summary.Cleanup, client.liveCount())
	}
}

func TestRunLaunchIgnoresEmptyListStartedBeforeSettle(t *testing.T) {
	client := newFakeLaunchClient()
	client.lateCreates.Store(1)
	client.lateBy = -1
	cfg := testLaunchConfig(4)
	cfg.HTTPTimeout = 200 * time.Millisecond
	// The first sweep snapshots an empty list before the settle window
	// ends, returns after it, and the late create lands in between.
	client.afterList = func(call int) {
		if call == 1 {
			time.Sleep(300 * time.Millisecond)
			client.landPending()
		}
	}
	summary, _ := launchRunner(cfg, client).RunLaunch(context.Background(), nil)
	if !summary.Cleanup.Verified || summary.Cleanup.Swept != 1 || client.liveCount() != 0 {
		t.Fatalf("cleanup verified from a list taken before the settle window ended: %+v live=%d", summary.Cleanup, client.liveCount())
	}
}

func TestRunLaunchInitialGuardAbortLaunchesNothing(t *testing.T) {
	client := newFakeLaunchClient()
	source := staticSource(PressureSourceGMP, freeMemory("host-a", 1), nil)
	monitor := NewPressureMonitor(testPressureConfig(), []PressureSource{source}, nil)
	summary, err := launchRunner(testLaunchConfig(20), client).RunLaunch(context.Background(), monitor)
	if !errors.Is(err, ErrGuardAbort) {
		t.Fatalf("RunLaunch error = %v, want guard abort", err)
	}
	if client.creates.Load() != 0 || summary.Dispatched != 0 || summary.Unstarted != 20 {
		t.Fatalf("launch created sandboxes despite an abort-level first reading: creates=%d %+v", client.creates.Load(), summary)
	}
}

func TestPacerDropsAbandonedReservations(t *testing.T) {
	p := newPacer(10)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		_ = p.wait(canceled)
	}
	p.dropReservations()
	start := time.Now()
	if err := p.wait(context.Background()); err != nil {
		t.Fatalf("wait returned %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("wait queued behind abandoned reservations for %s", elapsed)
	}
}

func TestFormatMilestonesUsesGlobalScale(t *testing.T) {
	// Task 1 of 2 dispatched 100 operations spanning global indexes 0-199;
	// a failure at index 198 belongs to the first-1000 bucket.
	got := formatMilestones([]FailureMilestone{{Operations: 1000, Failed: 1}}, 100, 1, 200)
	if got != "first 100: 0 | all 100: 1" {
		t.Fatalf("formatMilestones = %q", got)
	}
}

func TestRunLaunchGuardAbortStopsAndCleansUp(t *testing.T) {
	client := newFakeLaunchClient()
	cfg := testLaunchConfig(200)
	cfg.Launch.Rate = 100
	source := &fakePressureSource{name: PressureSourceGMP, read: func(call int) (PressureReading, error) {
		value := 50.0
		if call > 1 {
			value = 1
		}
		return PressureReading{Samples: []PressureSample{{Signal: "free_memory_pct", HostID: "host-a", Value: value}}}, nil
	}}
	pressure := testPressureConfig()
	pressure.Interval = 50 * time.Millisecond
	monitor := NewPressureMonitor(pressure, []PressureSource{source}, nil)
	summary, err := launchRunner(cfg, client).RunLaunch(context.Background(), monitor)
	if !errors.Is(err, ErrGuardAbort) {
		t.Fatalf("RunLaunch error = %v, want guard abort", err)
	}
	if summary.Created == 0 || summary.Unstarted == 0 || !strings.Contains(summary.StopReason, "guard") {
		t.Fatalf("guard did not stop the launch mid-run: %+v", summary)
	}
	if !summary.Cleanup.Verified || client.liveCount() != 0 {
		t.Fatalf("aborted launch not cleaned up: %+v live=%d", summary.Cleanup, client.liveCount())
	}
	if summary.Pressure.TriggerCounts["free_memory_pct/abort"] == 0 {
		t.Fatalf("abort trigger not reported: %+v", summary.Pressure)
	}
	report := summary.Report(cfg, err)
	for _, want := range []string{"Outcome: aborted", "free_memory_pct", "host-a"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

func TestRunLaunchUsesFanOutOperationIndexes(t *testing.T) {
	client := newFakeLaunchClient()
	cfg := testLaunchConfig(9)
	cfg.Concurrency = 3
	cfg.WorkerID = "task-1"
	cfg.Launch.TaskIndex, cfg.Launch.TaskCount = 1, 3
	summary, err := launchRunner(cfg, client).RunLaunch(context.Background(), nil)
	if err != nil {
		t.Fatalf("RunLaunch returned %v", err)
	}
	if summary.TaskOperations != 3 || summary.Created != 3 {
		t.Fatalf("unexpected task share: %+v", summary)
	}
	client.liveMu.Lock()
	defer client.liveMu.Unlock()
	names := strings.Join(client.names, ",")
	for _, operation := range []int{1, 4, 7} {
		if !strings.Contains(names, sandboxNameForOperation(cfg.RunID, cfg.WorkerID, operation)) {
			t.Fatalf("task 1 of 3 did not create operation %d: %s", operation, names)
		}
	}
}
