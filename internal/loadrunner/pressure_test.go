package loadrunner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePressureSource struct {
	name  string
	mu    sync.Mutex
	calls int
	read  func(call int) (PressureReading, error)
}

func (f *fakePressureSource) Name() string { return f.name }

func (f *fakePressureSource) Read(context.Context) (PressureReading, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	return f.read(call)
}

func staticSource(name string, reading PressureReading, err error) *fakePressureSource {
	return &fakePressureSource{name: name, read: func(int) (PressureReading, error) { return reading, err }}
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testPressureConfig() PressureConfig {
	return PressureConfig{
		Mode: PressureModeEnforce, Interval: time.Second, BackoffPause: 10 * time.Second, MaxBackoffs: 2,
		SourceTimeout: time.Second, CreateErrorWindow: 30 * time.Second, CreateErrorMinSamples: 20,
		Rules: defaultPressureRules(),
	}
}

func freeMemory(host string, value float64) PressureReading {
	return PressureReading{Samples: []PressureSample{{Signal: "free_memory_pct", HostID: host, Value: value}}}
}

func TestPressureMonitorBacksOffPausesAndRecovers(t *testing.T) {
	clock := &testClock{now: time.Unix(1_000, 0)}
	value := 8.0
	source := &fakePressureSource{name: PressureSourceGMP, read: func(int) (PressureReading, error) { return freeMemory("host-a", value), nil }}
	m := NewPressureMonitor(testPressureConfig(), []PressureSource{source}, clock.Now)

	m.Evaluate(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, paused, err := m.Gate(ctx); !paused || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Gate during backoff pause = paused %v err %v, want a blocking pause", paused, err)
	}
	clock.Advance(11 * time.Second)
	if multiplier, _, err := m.Gate(context.Background()); err != nil || multiplier != 0.5 {
		t.Fatalf("Gate after pause = %v, %v; want multiplier 0.5", multiplier, err)
	}

	value = 50
	m.Evaluate(context.Background())
	if multiplier, _, _ := m.Gate(context.Background()); multiplier != 1 {
		t.Fatalf("multiplier after a clear evaluation = %v, want 1", multiplier)
	}
	report := m.Report()
	if report.Backoffs != 1 || report.MinMultiplier != 0.5 || report.PausedFor != 10*time.Second || report.TriggerCounts["free_memory_pct/backoff"] != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.Triggers) != 1 || report.Triggers[0].HostID != "host-a" || report.Triggers[0].Threshold != 10 || !report.Triggers[0].Enforced {
		t.Fatalf("unexpected trigger: %+v", report.Triggers)
	}
	if len(report.Peaks) != 1 || report.Peaks[0].Worst != 8 || report.Peaks[0].Last != 50 {
		t.Fatalf("unexpected peaks: %+v", report.Peaks)
	}
	if m.Err() != nil {
		t.Fatalf("guard aborted unexpectedly: %v", m.Err())
	}
}

func TestPressureMonitorAbortsOnThreshold(t *testing.T) {
	m := NewPressureMonitor(testPressureConfig(), []PressureSource{staticSource(PressureSourceGMP, freeMemory("host-a", 2), nil)}, nil)
	m.Evaluate(context.Background())
	if !errors.Is(m.Err(), ErrGuardAbort) {
		t.Fatalf("Err = %v, want ErrGuardAbort", m.Err())
	}
	if _, _, err := m.Gate(context.Background()); !errors.Is(err, ErrGuardAbort) {
		t.Fatalf("Gate after abort = %v", err)
	}
	if report := m.Report(); report.Aborted == "" || report.TriggerCounts["free_memory_pct/abort"] != 1 {
		t.Fatalf("abort not reported: %+v", report)
	}
}

func TestPressureMonitorEscalatesRepeatedBackoffs(t *testing.T) {
	cfg := testPressureConfig()
	cfg.BackoffPause = 0
	m := NewPressureMonitor(cfg, []PressureSource{staticSource(PressureSourceGMP, freeMemory("host-a", 8), nil)}, nil)
	for range cfg.MaxBackoffs {
		m.Evaluate(context.Background())
		if m.Err() != nil {
			t.Fatalf("aborted before MaxBackoffs: %v", m.Err())
		}
	}
	m.Evaluate(context.Background())
	if !errors.Is(m.Err(), ErrGuardAbort) || !strings.Contains(m.Err().Error(), "consecutive backoffs") {
		t.Fatalf("Err = %v, want escalation abort", m.Err())
	}
	if report := m.Report(); report.TriggerCounts["backoff_escalation/abort"] != 1 || report.MinMultiplier != 0.25 {
		t.Fatalf("unexpected escalation report: %+v", report)
	}
}

func TestPressureMonitorObserveModeOnlyRecords(t *testing.T) {
	cfg := testPressureConfig()
	cfg.Mode = PressureModeObserve
	m := NewPressureMonitor(cfg, []PressureSource{staticSource(PressureSourceGMP, freeMemory("host-a", 1), nil)}, nil)
	m.Evaluate(context.Background())
	if m.Err() != nil {
		t.Fatalf("observe mode aborted: %v", m.Err())
	}
	if multiplier, paused, err := m.Gate(context.Background()); multiplier != 1 || paused || err != nil {
		t.Fatalf("observe mode throttled: %v %v %v", multiplier, paused, err)
	}
	report := m.Report()
	if len(report.Triggers) != 1 || report.Triggers[0].Enforced || report.Triggers[0].Action != GuardActionAbort {
		t.Fatalf("observe trigger not recorded: %+v", report.Triggers)
	}
}

func TestPressureMonitorFailsClosedOnSourceErrors(t *testing.T) {
	cfg := testPressureConfig()
	cfg.BackoffPause = 0
	m := NewPressureMonitor(cfg, []PressureSource{staticSource(PressureSourceSupabase, PressureReading{}, errors.New("unexpected status 503"))}, nil)
	m.Evaluate(context.Background())
	report := m.Report()
	if report.SourceErrors[PressureSourceSupabase] != 1 || report.TriggerCounts["source_unavailable/backoff"] != 1 || report.Backoffs != 1 {
		t.Fatalf("unavailable source did not back off: %+v", report)
	}
	for range cfg.MaxBackoffs {
		m.Evaluate(context.Background())
	}
	if !errors.Is(m.Err(), ErrGuardAbort) {
		t.Fatalf("persistently unavailable source did not abort: %v", m.Err())
	}
}

func TestPressureMonitorGeneratorSignals(t *testing.T) {
	clock := &testClock{now: time.Unix(1_000, 0)}
	cfg := testPressureConfig()
	cfg.BackoffPause = 0
	m := NewPressureMonitor(cfg, nil, clock.Now)
	for i := range 19 {
		m.ObserveCreate(clock.Now(), time.Second, i < 5)
	}
	m.Evaluate(context.Background())
	if report := m.Report(); len(report.Triggers) != 0 {
		t.Fatalf("triggered below the minimum sample count: %+v", report.Triggers)
	}
	m.ObserveCreate(clock.Now(), time.Second, false)
	m.Evaluate(context.Background())
	report := m.Report()
	if report.TriggerCounts["create_error_ratio/backoff"] != 1 || m.Err() != nil {
		t.Fatalf("create error ratio 0.25 should back off only: %+v err=%v", report, m.Err())
	}
	clock.Advance(time.Minute)
	m.Evaluate(context.Background())
	if multiplier, _, _ := m.Gate(context.Background()); multiplier != 1 {
		t.Fatalf("old create samples were not dropped; multiplier %v", multiplier)
	}
	for range 20 {
		m.ObserveCreate(clock.Now(), 120*time.Second, false)
	}
	m.Evaluate(context.Background())
	if !errors.Is(m.Err(), ErrGuardAbort) || m.Report().TriggerCounts["create_p99_seconds/abort"] != 1 {
		t.Fatalf("slow creates did not abort: %v %+v", m.Err(), m.Report().TriggerCounts)
	}
}

func TestPressureMonitorHostAllowlistIgnoresOtherHosts(t *testing.T) {
	cfg := testPressureConfig()
	cfg.HostIDs = []string{"host-a"}
	reading := PressureReading{
		Samples: []PressureSample{
			{Signal: "free_memory_pct", HostID: "retired-host", Value: 1},
			{Signal: "free_memory_pct", HostID: "host-a", Value: 60},
			{Signal: "db_pool_utilization", Value: 0.5},
		},
		Hosts: []HostPressure{
			{HostID: "retired-host", MaxSandboxes: 10, RunningSandboxes: 10},
			{HostID: "host-a", MaxSandboxes: 100, RunningSandboxes: 10},
		},
	}
	m := NewPressureMonitor(cfg, []PressureSource{staticSource(PressureSourceSupabase, reading, nil)}, nil)
	m.Evaluate(context.Background())
	report := m.Report()
	if m.Err() != nil || len(report.Triggers) != 0 {
		t.Fatalf("stale host affected the guard: err=%v triggers=%+v", m.Err(), report.Triggers)
	}
	if len(report.Hosts) != 1 || report.Hosts[0].HostID != "host-a" || report.Hosts[0].MinHeadroomPct != 90 {
		t.Fatalf("unexpected host caps: %+v", report.Hosts)
	}
	if len(report.Peaks) != 2 || m.AllHostsFull() {
		t.Fatalf("unexpected peaks or full state: %+v full=%v", report.Peaks, m.AllHostsFull())
	}
}

func TestPressureReportListsUnreadSignals(t *testing.T) {
	cfg := testPressureConfig()
	m := NewPressureMonitor(cfg, []PressureSource{staticSource(PressureSourceSupabase, PressureReading{}, nil)}, nil)
	unconfigured := strings.Join(m.Report().Unconfigured, ",")
	for _, signal := range []string{"free_memory_pct", "memory_psi_avg10", "restore_p99_seconds"} {
		if !strings.Contains(unconfigured, signal) {
			t.Fatalf("Unconfigured %q missing %s", unconfigured, signal)
		}
	}
	if strings.Contains(unconfigured, "cap_headroom_pct") || strings.Contains(unconfigured, "create_error_ratio") {
		t.Fatalf("Unconfigured lists active signals: %q", unconfigured)
	}
	if NewPressureMonitor(PressureConfig{Mode: PressureModeOff}, nil, nil) != nil {
		t.Fatal("off mode should return a nil monitor")
	}
	var nilMonitor *PressureMonitor
	if multiplier, _, err := nilMonitor.Gate(context.Background()); multiplier != 1 || err != nil || nilMonitor.Report().Mode != PressureModeOff {
		t.Fatal("nil monitor should behave as a disabled guard")
	}
}

func TestHostPressureCaps(t *testing.T) {
	tests := []struct {
		name     string
		host     HostPressure
		limit    int
		full     bool
		headroom float64
	}{
		{name: "no caps", host: HostPressure{RunningSandboxes: 500}, limit: 0, full: false, headroom: -1},
		{name: "sandbox cap", host: HostPressure{MaxSandboxes: 100, RunningSandboxes: 90, ProvisioningSandboxes: 5, PausedSandboxes: 5}, limit: 0, full: true, headroom: 0},
		{name: "operator slot limit below ceiling", host: HostPressure{MaxNetworkSlots: 200, NetSlotCeiling: 1000, UsedNetSlots: 150}, limit: 200, full: false, headroom: 25},
		{name: "ceiling only", host: HostPressure{NetSlotCeiling: 100, UsedNetSlots: 100}, limit: 100, full: true, headroom: 0},
		{name: "warm slot available", host: HostPressure{NetSlotCeiling: 100, UsedNetSlots: 98, WarmNetSlots: 2}, limit: 100, full: false, headroom: 0},
		{name: "over sandbox cap with free slots", host: HostPressure{MaxSandboxes: 100, RunningSandboxes: 120, NetSlotCeiling: 1000, UsedNetSlots: 120}, limit: 1000, full: true, headroom: 0},
		{name: "tightest cap wins", host: HostPressure{MaxSandboxes: 1000, RunningSandboxes: 100, MaxNetworkSlots: 200, UsedNetSlots: 180}, limit: 200, full: false, headroom: 10},
	}
	for _, tt := range tests {
		if got := tt.host.SlotLimit(); got != tt.limit {
			t.Errorf("%s: SlotLimit = %d, want %d", tt.name, got, tt.limit)
		}
		if got := tt.host.Full(); got != tt.full {
			t.Errorf("%s: Full = %v, want %v", tt.name, got, tt.full)
		}
		if got := tt.host.HeadroomPct(); got != tt.headroom {
			t.Errorf("%s: HeadroomPct = %v, want %v", tt.name, got, tt.headroom)
		}
	}
}

func TestSupabaseHostPressureSource(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/rest/v1/host_pressure" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("apikey") != "service-key" || r.Header.Get("Authorization") != "Bearer service-key" {
			t.Error("missing Supabase credentials")
		}
		gotQuery = r.URL.Query().Get("host_id")
		_ = json.NewEncoder(w).Encode([]HostPressure{{HostID: "host-a", MaxSandboxes: 100, RunningSandboxes: 40, ReportedAt: now.Add(-30 * time.Second)}})
	}))
	defer server.Close()
	source := SupabaseHostPressureSource{URL: server.URL, Key: "service-key", HostIDs: []string{"host-a", "host-b"}, HTTP: server.Client(), Now: func() time.Time { return now }}
	// host-b has no row, so its caps are unknown and the read fails closed
	// while still returning host-a's values.
	reading, err := source.Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "host-b") {
		t.Fatalf("Read error = %v, want missing host-b", err)
	}
	if gotQuery != "in.(host-a,host-b)" {
		t.Fatalf("host_id filter = %q", gotQuery)
	}
	if len(reading.Hosts) != 1 || len(reading.Samples) != 2 {
		t.Fatalf("unexpected reading: %+v", reading)
	}
	for _, sample := range reading.Samples {
		if sample.Signal == "cap_headroom_pct" && sample.Value != 60 || sample.Signal == "host_report_age_seconds" && sample.Value != 30 {
			t.Fatalf("unexpected sample: %+v", sample)
		}
	}
}

func TestPressureSourcesFailClosedWithoutData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/v1/host_pressure" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()
	supabase := SupabaseHostPressureSource{URL: server.URL, Key: "service-key", HTTP: server.Client()}
	if _, err := supabase.Read(context.Background()); err == nil {
		t.Fatal("empty host_pressure read succeeded")
	}
	gmp := GMPSource{Project: "example-project", Environment: "staging", Rules: defaultPressureRules(), HTTP: server.Client(), BaseURL: server.URL}
	reading, err := gmp.Read(context.Background())
	if err == nil {
		t.Fatal("GMP read with no data for any guarded query succeeded")
	}
	if !slices.Contains(reading.NoData, "restore_p99_seconds") || slices.Contains(reading.NoData, "restore_rate") {
		t.Fatalf("NoData = %v, want guarded signals only", reading.NoData)
	}
}

func TestGMPSourceIgnoresDataForExcludedHosts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"host_id":"host-b"},"value":[1700000000,"1"]}]}}`))
	}))
	defer server.Close()
	rules := []PressureRule{
		{Signal: "restore_p99_seconds", Source: PressureSourceGMP, Backoff: 15, Query: "guarded"},
		{Signal: "restore_rate", Source: PressureSourceGMP, Query: "report-only"},
	}
	gmp := GMPSource{Project: "example-project", Environment: "staging", Rules: rules, HostIDs: []string{"host-a"}, HTTP: server.Client(), BaseURL: server.URL}
	reading, err := gmp.Read(context.Background())
	if err == nil || len(reading.Samples) != 0 || !slices.Equal(reading.NoData, []string{"restore_p99_seconds"}) {
		t.Fatalf("Read = %+v, %v; want no data for host-a", reading, err)
	}
}

func TestPressureSourceErrorsHideBodiesAndKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"secret-body service-key"}`))
	}))
	defer server.Close()
	supabase := SupabaseHostPressureSource{URL: server.URL, Key: "service-key", HTTP: server.Client()}
	gmp := GMPSource{Project: "example-project", Environment: "staging", Rules: defaultPressureRules(), HTTP: server.Client(), BaseURL: server.URL}
	for _, source := range []PressureSource{supabase, gmp} {
		_, err := source.Read(context.Background())
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("%s: error = %v, want status 403", source.Name(), err)
		}
		if strings.Contains(err.Error(), "secret-body") || strings.Contains(err.Error(), "service-key") {
			t.Fatalf("%s: error leaked response body or key: %v", source.Name(), err)
		}
	}
}

func TestGMPSourceQueriesConfiguredRules(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/example-project/location/global/prometheus/api/v1/query" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		queries = append(queries, r.URL.Query().Get("query"))
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"host_id":"host-a"},"value":[1700000000,"12.5"]},
			{"metric":{"host_id":"host-b"},"value":[1700000000,"NaN"]}]}}`))
	}))
	defer server.Close()
	rules := []PressureRule{
		{Signal: "restore_p99_seconds", Source: PressureSourceGMP, Query: `q{environment="$ENV"}`},
		{Signal: "free_memory_pct", Source: PressureSourceGMP},
		{Signal: "cap_headroom_pct", Source: PressureSourceSupabase, Query: "ignored"},
	}
	source := GMPSource{Project: "example-project", Environment: "staging", Rules: rules, HTTP: server.Client(), BaseURL: server.URL}
	reading, err := source.Read(context.Background())
	if err != nil {
		t.Fatalf("Read returned %v", err)
	}
	if len(queries) != 1 || queries[0] != `q{environment="staging"}` {
		t.Fatalf("unexpected queries: %q", queries)
	}
	if len(reading.Samples) != 1 || reading.Samples[0] != (PressureSample{Signal: "restore_p99_seconds", HostID: "host-a", Value: 12.5}) {
		t.Fatalf("unexpected samples: %+v", reading.Samples)
	}
}

func TestNewPressureSourcesBuildsConfiguredSources(t *testing.T) {
	if sources := NewPressureSources(PressureConfig{}, nil, nil, nil); len(sources) != 0 {
		t.Fatalf("unconfigured guard built sources: %+v", sources)
	}
	sources := NewPressureSources(PressureConfig{SupabaseURL: "https://example.supabase.co", SupabaseKey: "key", GMPProject: "example-project"}, nil, nil, nil)
	if len(sources) != 2 || sources[0].Name() != PressureSourceSupabase || sources[1].Name() != PressureSourceGMP {
		t.Fatalf("unexpected sources: %+v", sources)
	}
}
