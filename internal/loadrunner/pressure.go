package loadrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	PressureModeOff     = "off"
	PressureModeObserve = "observe"
	PressureModeEnforce = "enforce"

	GuardActionBackoff = "backoff"
	GuardActionAbort   = "abort"

	PressureSourceGMP       = "gmp"
	PressureSourceSupabase  = "supabase"
	PressureSourceGenerator = "generator"
)

const (
	minRateMultiplier      = 1.0 / 16
	maxRecordedTriggers    = 200
	maxHostPressureRows    = 64
	maxPressureBodyBytes   = 1 << 20
	gmpEnvironmentTemplate = "$ENV"
)

// PressureRule is one guarded signal. Backoff and Abort are thresholds; zero
// disables that action, and a rule with both disabled is report-only.
// LowerIsWorse marks signals such as free memory where small values are bad.
type PressureRule struct {
	Signal       string
	Source       string
	Query        string
	LowerIsWorse bool
	Backoff      float64
	Abort        float64
}

type PressureConfig struct {
	Mode                  string
	Interval              time.Duration
	BackoffPause          time.Duration
	MaxBackoffs           int
	SourceTimeout         time.Duration
	SupabaseURL           string
	SupabaseKey           string
	GMPProject            string
	GMPEnvironment        string
	CreateErrorWindow     time.Duration
	CreateErrorMinSamples int
	// GMPPresenceQuery returns one series per host whose exporter is
	// reporting. It proves the per-host signals cover the watched hosts.
	GMPPresenceQuery string
	// HostIDs limits host signals to the hosts under test. Empty means every
	// host, which also counts stale rows for retired hosts.
	HostIDs []string
	Rules   []PressureRule
}

// HasHostSource reports whether the guard reads real host state rather than
// only the generator's own create outcomes: a configured source must have at
// least one signal with a threshold, and GMP must also check host presence.
func (c PressureConfig) HasHostSource() bool {
	guarded := func(source string) bool {
		for _, rule := range c.Rules {
			if rule.Source == source && rule.guarded() && (source != PressureSourceGMP || rule.Query != "") {
				return true
			}
		}
		return false
	}
	return c.SupabaseURL != "" && c.SupabaseKey != "" && guarded(PressureSourceSupabase) ||
		c.GMPProject != "" && c.GMPPresenceQuery != "" && guarded(PressureSourceGMP)
}

func (r PressureRule) guarded() bool { return r.Backoff > 0 || r.Abort > 0 }

// NewPressureSources builds the configured host sources. gmpHTTP must carry
// monitoring.read credentials and is only used when a GMP project is set.
func NewPressureSources(cfg PressureConfig, supabaseHTTP, gmpHTTP *http.Client, now func() time.Time) []PressureSource {
	var sources []PressureSource
	if cfg.SupabaseURL != "" && cfg.SupabaseKey != "" {
		sources = append(sources, SupabaseHostPressureSource{URL: cfg.SupabaseURL, Key: cfg.SupabaseKey, HostIDs: cfg.HostIDs, HTTP: supabaseHTTP, Now: now})
	}
	if cfg.GMPProject != "" {
		sources = append(sources, GMPSource{Project: cfg.GMPProject, Environment: cfg.GMPEnvironment, PresenceQuery: cfg.GMPPresenceQuery, Rules: cfg.Rules, HostIDs: cfg.HostIDs, HTTP: gmpHTTP})
	}
	return sources
}

// vmd samples conntrack on every host every 5s, whatever its feature flags,
// so recent host_conntrack_max series list the hosts whose exporters are
// alive.
const defaultGMPPresenceQuery = `count by (host_id) (last_over_time(host_conntrack_max{environment="$ENV"}[2m]))`

// defaultPressureRules lists every signal the guard understands. Host memory,
// PSI, CPU steal and run queue have no default query because no exported
// metric carries them yet; set LOAD_TEST_GUARD_<SIGNAL>_QUERY to enable them.
// $ENV in a query is replaced with the GMP environment label value.
func defaultPressureRules() []PressureRule {
	return []PressureRule{
		{Signal: "free_memory_pct", Source: PressureSourceGMP, LowerIsWorse: true, Backoff: 10, Abort: 5},
		{Signal: "memory_psi_avg10", Source: PressureSourceGMP, Backoff: 10, Abort: 40},
		{Signal: "cpu_steal_pct", Source: PressureSourceGMP, Backoff: 10, Abort: 30},
		{Signal: "run_queue_per_cpu", Source: PressureSourceGMP, Backoff: 2, Abort: 5},
		{Signal: "restore_p99_seconds", Source: PressureSourceGMP, Backoff: 15, Abort: 45,
			Query: `histogram_quantile(0.99, sum by (le, host_id) (rate(vmd_call_duration_seconds_bucket{environment="$ENV",method="CreateVM"}[1m])))`},
		{Signal: "restore_error_ratio", Source: PressureSourceGMP, Backoff: 0.05, Abort: 0.25,
			Query: `sum by (host_id) (rate(vmd_call_total{environment="$ENV",method="CreateVM",result="error"}[1m])) / clamp_min(sum by (host_id) (rate(vmd_call_total{environment="$ENV",method="CreateVM"}[1m])), 1e-9)`},
		{Signal: "db_pool_utilization", Source: PressureSourceGMP, Backoff: 0.85, Abort: 0.97,
			Query: `max(db_pool_acquired_conns{environment="$ENV"} / clamp_min(db_pool_total_conns{environment="$ENV"}, 1))`},
		{Signal: "restore_rate", Source: PressureSourceGMP,
			Query: `sum by (host_id) (rate(vmd_call_total{environment="$ENV",method="CreateVM",result="success"}[1m]))`},
		{Signal: "control_plane_transition_rate", Source: PressureSourceGMP,
			Query: `sum(rate(sandbox_transition_total{environment="$ENV"}[1m]))`},
		{Signal: "db_pool_empty_acquire_rate", Source: PressureSourceGMP,
			Query: `sum(rate(db_pool_empty_acquire_total{environment="$ENV"}[1m]))`},
		{Signal: "cap_headroom_pct", Source: PressureSourceSupabase, LowerIsWorse: true, Backoff: 10, Abort: 2},
		{Signal: "host_report_age_seconds", Source: PressureSourceSupabase, Backoff: 90, Abort: 300},
		{Signal: "create_error_ratio", Source: PressureSourceGenerator, Backoff: 0.05, Abort: 0.25},
		{Signal: "create_p99_seconds", Source: PressureSourceGenerator, Backoff: 30, Abort: 90},
	}
}

func loadPressureConfig(environment string) (PressureConfig, error) {
	cfg := PressureConfig{
		Mode:                  envDefault("LOAD_TEST_GUARD_MODE", PressureModeEnforce),
		Interval:              envDuration("LOAD_TEST_GUARD_INTERVAL", 10*time.Second),
		BackoffPause:          envDuration("LOAD_TEST_GUARD_BACKOFF_PAUSE", 15*time.Second),
		MaxBackoffs:           envInt("LOAD_TEST_GUARD_MAX_BACKOFFS", 4),
		SourceTimeout:         envDuration("LOAD_TEST_GUARD_SOURCE_TIMEOUT", 5*time.Second),
		SupabaseURL:           strings.TrimRight(strings.TrimSpace(os.Getenv("LOAD_TEST_PRESSURE_SUPABASE_URL")), "/"),
		SupabaseKey:           strings.TrimSpace(os.Getenv("LOAD_TEST_PRESSURE_SUPABASE_KEY")),
		GMPProject:            strings.TrimSpace(os.Getenv("LOAD_TEST_GUARD_GMP_PROJECT")),
		GMPEnvironment:        envDefault("LOAD_TEST_GUARD_GMP_ENVIRONMENT", environment),
		GMPPresenceQuery:      envDefault("LOAD_TEST_GUARD_GMP_PRESENCE_QUERY", defaultGMPPresenceQuery),
		CreateErrorWindow:     envDuration("LOAD_TEST_GUARD_CREATE_WINDOW", 30*time.Second),
		CreateErrorMinSamples: envInt("LOAD_TEST_GUARD_CREATE_MIN_SAMPLES", 20),
	}
	for _, id := range strings.Split(os.Getenv("LOAD_TEST_GUARD_HOST_IDS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.HostIDs = append(cfg.HostIDs, id)
		}
	}
	for _, rule := range defaultPressureRules() {
		prefix := "LOAD_TEST_GUARD_" + strings.ToUpper(rule.Signal) + "_"
		if rule.Source == PressureSourceGMP {
			if query, ok := os.LookupEnv(prefix + "QUERY"); ok {
				rule.Query = strings.TrimSpace(query)
			}
		}
		rule.Backoff = envFloat(prefix+"BACKOFF", rule.Backoff)
		rule.Abort = envFloat(prefix+"ABORT", rule.Abort)
		if rule.Backoff < 0 || rule.Abort < 0 {
			return PressureConfig{}, fmt.Errorf("%sBACKOFF and %sABORT must be non-negative numbers", prefix, prefix)
		}
		cfg.Rules = append(cfg.Rules, rule)
	}
	return cfg, nil
}

func validatePressureConfig(cfg PressureConfig) error {
	switch cfg.Mode {
	case PressureModeOff, PressureModeObserve, PressureModeEnforce:
	default:
		return errors.New("LOAD_TEST_GUARD_MODE must be off, observe, or enforce")
	}
	if cfg.Mode == PressureModeOff {
		return nil
	}
	if cfg.Interval < time.Second || cfg.Interval > 5*time.Minute || cfg.BackoffPause < 0 || cfg.BackoffPause > 10*time.Minute ||
		cfg.MaxBackoffs < 1 || cfg.SourceTimeout <= 0 || cfg.SourceTimeout > cfg.Interval ||
		cfg.CreateErrorWindow < time.Second || cfg.CreateErrorMinSamples < 1 {
		return errors.New("guard interval, backoff pause, max backoffs, source timeout and create window must be within their safety bounds")
	}
	if (cfg.SupabaseURL == "") != (cfg.SupabaseKey == "") {
		return errors.New("LOAD_TEST_PRESSURE_SUPABASE_URL and LOAD_TEST_PRESSURE_SUPABASE_KEY must be set together")
	}
	if cfg.SupabaseURL != "" {
		u, err := url.Parse(cfg.SupabaseURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("LOAD_TEST_PRESSURE_SUPABASE_URL must be an HTTPS origin")
		}
	}
	if cfg.GMPProject != "" && !validRoutingToken(cfg.GMPProject) {
		return errors.New("LOAD_TEST_GUARD_GMP_PROJECT is not a valid project ID")
	}
	// The environment is interpolated into PromQL label matchers.
	if cfg.GMPProject != "" && !validRoutingToken(cfg.GMPEnvironment) {
		return errors.New("LOAD_TEST_GUARD_GMP_ENVIRONMENT must be a plain environment label value")
	}
	if len(cfg.HostIDs) > maxHostPressureRows {
		return fmt.Errorf("LOAD_TEST_GUARD_HOST_IDS accepts at most %d hosts", maxHostPressureRows)
	}
	for _, id := range cfg.HostIDs {
		if !validRoutingToken(id) {
			return errors.New("LOAD_TEST_GUARD_HOST_IDS must be comma-separated host IDs")
		}
	}
	for _, rule := range cfg.Rules {
		if rule.Backoff > 0 && rule.Abort > 0 && (rule.LowerIsWorse && rule.Abort > rule.Backoff || !rule.LowerIsWorse && rule.Abort < rule.Backoff) {
			return fmt.Errorf("guard %s abort threshold must be at least as severe as its backoff threshold", rule.Signal)
		}
	}
	return nil
}

// PressureSample is one signal value; HostID is empty for fleet-wide values.
type PressureSample struct {
	Signal string
	HostID string
	Value  float64
}

// HostPressure mirrors one host_pressure row as reported by vmd.
type HostPressure struct {
	HostID                string    `json:"host_id"`
	RunningSandboxes      int       `json:"running_sandboxes"`
	ProvisioningSandboxes int       `json:"provisioning_sandboxes"`
	PausedSandboxes       int       `json:"paused_sandboxes"`
	AllocatedMemoryMiB    int64     `json:"allocated_memory_mib"`
	AllocatedVCPUs        int64     `json:"allocated_vcpus"`
	UsedNetSlots          int       `json:"used_net_slots"`
	ProvisioningNetSlots  int       `json:"provisioning_net_slots"`
	WarmNetSlots          int       `json:"warm_net_slots"`
	NetSlotCeiling        int       `json:"net_slot_ceiling"`
	MaxNetworkSlots       int       `json:"max_network_slots"`
	MaxSandboxes          int       `json:"max_sandboxes"`
	ReportedAt            time.Time `json:"reported_at"`
}

// SlotLimit matches the scheduler: the operator limit and the technical
// ceiling both bound slots, and either may be unset (zero).
func (h HostPressure) SlotLimit() int {
	switch {
	case h.MaxNetworkSlots > 0 && h.NetSlotCeiling > 0:
		return min(h.MaxNetworkSlots, h.NetSlotCeiling)
	case h.MaxNetworkSlots > 0:
		return h.MaxNetworkSlots
	default:
		return h.NetSlotCeiling
	}
}

// Full reports whether the scheduler's capacity check would reject one more
// sandbox on this host.
func (h HostPressure) Full() bool {
	if h.MaxSandboxes > 0 && h.RunningSandboxes+h.ProvisioningSandboxes+h.PausedSandboxes+1 > h.MaxSandboxes {
		return true
	}
	limit := h.SlotLimit()
	return limit > 0 && h.UsedNetSlots+h.ProvisioningNetSlots+h.WarmNetSlots+max(0, 1-h.WarmNetSlots) > limit
}

// HeadroomPct is the smallest remaining share of any configured cap, or -1
// when the host publishes no cap.
func (h HostPressure) HeadroomPct() float64 {
	headroom := -1.0
	if h.MaxSandboxes > 0 {
		used := h.RunningSandboxes + h.ProvisioningSandboxes + h.PausedSandboxes
		headroom = max(0, 100*float64(h.MaxSandboxes-used)/float64(h.MaxSandboxes))
	}
	if limit := h.SlotLimit(); limit > 0 {
		used := h.UsedNetSlots + h.ProvisioningNetSlots + h.WarmNetSlots
		slots := max(0, 100*float64(limit-used)/float64(limit))
		if headroom < 0 || slots < headroom {
			headroom = slots
		}
	}
	return headroom
}

type PressureReading struct {
	Samples []PressureSample
	Hosts   []HostPressure
	// NoData lists guarded signals whose query returned no usable value.
	NoData []string
}

type PressureSource interface {
	Name() string
	Read(context.Context) (PressureReading, error)
}

// SupabaseHostPressureSource reads host_pressure through PostgREST. It makes
// one bounded GET per guard interval and never writes.
type SupabaseHostPressureSource struct {
	URL     string
	Key     string
	HostIDs []string
	HTTP    *http.Client
	Now     func() time.Time
}

func (s SupabaseHostPressureSource) Name() string { return PressureSourceSupabase }

func (s SupabaseHostPressureSource) Read(ctx context.Context) (PressureReading, error) {
	// One extra row reveals a fleet larger than the guard can watch.
	endpoint := fmt.Sprintf("%s/rest/v1/host_pressure?select=*&order=host_id.asc&limit=%d", s.URL, maxHostPressureRows+1)
	if len(s.HostIDs) > 0 {
		endpoint += "&host_id=" + url.QueryEscape("in.("+strings.Join(s.HostIDs, ",")+")")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return PressureReading{}, errors.New("building host_pressure request failed")
	}
	req.Header.Set("apikey", s.Key)
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Accept", "application/json")
	var rows []HostPressure
	if err := doPressureJSON(s.HTTP, req, &rows); err != nil {
		return PressureReading{}, fmt.Errorf("reading host_pressure: %w", err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	reading := PressureReading{Hosts: rows}
	// A missing row hides that host's caps, so it fails closed like an
	// unreachable source.
	var readErr error
	if len(rows) == 0 {
		readErr = errors.New("host_pressure returned no rows")
	} else if len(rows) > maxHostPressureRows {
		rows = rows[:maxHostPressureRows]
		reading.Hosts = rows
		readErr = fmt.Errorf("host_pressure has more than %d rows; set LOAD_TEST_GUARD_HOST_IDS to the hosts under test", maxHostPressureRows)
	} else if missing := missingHosts(s.HostIDs, rows); len(missing) > 0 {
		readErr = fmt.Errorf("host_pressure has no row for %s", strings.Join(missing, ","))
	}
	for _, row := range rows {
		if headroom := row.HeadroomPct(); headroom >= 0 {
			reading.Samples = append(reading.Samples, PressureSample{Signal: "cap_headroom_pct", HostID: row.HostID, Value: headroom})
		}
		if !row.ReportedAt.IsZero() {
			reading.Samples = append(reading.Samples, PressureSample{Signal: "host_report_age_seconds", HostID: row.HostID, Value: math.Max(0, now().Sub(row.ReportedAt).Seconds())})
		}
	}
	return reading, readErr
}

func missingHosts(want []string, rows []HostPressure) []string {
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		seen[row.HostID] = true
	}
	var missing []string
	for _, id := range want {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

// GMPSource evaluates PromQL instant queries against Google Managed
// Prometheus. HTTP must carry monitoring.read credentials.
type GMPSource struct {
	Project     string
	Environment string
	// PresenceQuery lists the hosts whose exporters report. Each watched
	// host must appear, or every host when HostIDs is empty.
	PresenceQuery string
	Rules         []PressureRule
	// HostIDs drops per-host samples for other hosts, so data that only
	// covers excluded hosts counts as no data. Fleet-wide samples are kept.
	HostIDs []string
	HTTP    *http.Client
	BaseURL string
}

func (s GMPSource) Name() string { return PressureSourceGMP }

func (s GMPSource) Read(ctx context.Context) (PressureReading, error) {
	base := s.BaseURL
	if base == "" {
		base = "https://monitoring.googleapis.com"
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/location/global/prometheus/api/v1/query", strings.TrimRight(base, "/"), url.PathEscape(s.Project))
	query := func(signal, promQL string) ([]PressureSample, error) {
		promQL = strings.ReplaceAll(promQL, gmpEnvironmentTemplate, s.Environment)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+url.Values{"query": {promQL}}.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("%s: building query failed", signal)
		}
		var resp promQueryResponse
		if err := doPressureJSON(s.HTTP, req, &resp); err != nil {
			return nil, fmt.Errorf("%s: %w", signal, err)
		}
		samples, err := resp.samples(signal)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", signal, err)
		}
		return samples, nil
	}
	var reading PressureReading
	var errs []error
	guarded, guardedWithData := 0, 0
	for _, rule := range s.Rules {
		if rule.Source != PressureSourceGMP || rule.Query == "" {
			continue
		}
		if rule.guarded() {
			guarded++
		}
		samples, err := query(rule.Signal, rule.Query)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		samples = slices.DeleteFunc(samples, func(sample PressureSample) bool {
			return sample.HostID != "" && len(s.HostIDs) > 0 && !slices.Contains(s.HostIDs, sample.HostID)
		})
		reading.Samples = append(reading.Samples, samples...)
		if rule.guarded() {
			if len(samples) > 0 {
				guardedWithData++
			} else {
				reading.NoData = append(reading.NoData, rule.Signal)
			}
		}
	}
	// Rate and latency queries are legitimately empty while hosts are idle,
	// so one empty signal is only reported. Every guarded query empty means
	// the guard is blind (wrong project, environment, or exporter outage).
	if guarded > 0 && guardedWithData == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("no guarded query returned data"))
	}
	// Data from some hosts says nothing about a host whose exporter is
	// down, so every watched host must be reporting.
	if guarded > 0 {
		if err := s.checkPresence(query); err != nil {
			errs = append(errs, err)
		}
	}
	return reading, errors.Join(errs...)
}

func (s GMPSource) checkPresence(query func(signal, promQL string) ([]PressureSample, error)) error {
	if s.PresenceQuery == "" {
		return errors.New("no host presence query configured")
	}
	samples, err := query("host_presence", s.PresenceQuery)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(samples))
	for _, sample := range samples {
		if sample.HostID != "" {
			seen[sample.HostID] = true
		}
	}
	if len(seen) == 0 {
		return errors.New("host_presence: no host is reporting")
	}
	var missing []string
	for _, id := range s.HostIDs {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("host_presence: no metrics from %s", strings.Join(missing, ","))
	}
	return nil
}

type promQueryResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func (r promQueryResponse) samples(signal string) ([]PressureSample, error) {
	if r.Status != "success" {
		return nil, fmt.Errorf("query status %q", r.Status)
	}
	if r.Data.ResultType != "vector" && r.Data.ResultType != "scalar" {
		return nil, fmt.Errorf("unsupported result type %q", r.Data.ResultType)
	}
	var out []PressureSample
	for _, item := range r.Data.Result {
		if len(item.Value) != 2 {
			continue
		}
		raw, ok := item.Value[1].(string)
		if !ok {
			continue
		}
		// NaN is an idle 0/0 (for example a quantile with no requests). An
		// infinity is real: histogram_quantile returns +Inf past the top
		// bucket, so it must reach the thresholds.
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) {
			continue
		}
		out = append(out, PressureSample{Signal: signal, HostID: item.Metric["host_id"], Value: value})
	}
	return out, nil
}

// doPressureJSON keeps response bodies and credentials out of errors.
func doPressureJSON(client *http.Client, req *http.Request, out any) error {
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("request failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPressureBodyBytes))
	if err != nil {
		return errors.New("reading response failed")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("decoding response failed")
	}
	return nil
}

// GuardTrigger records one threshold crossing. Enforced is false in observe
// mode, where the action is logged but not applied.
type GuardTrigger struct {
	At        time.Time
	Signal    string
	HostID    string
	Value     float64
	Threshold float64
	Action    string
	Enforced  bool
}

type SignalPeak struct {
	Signal  string
	HostID  string
	Worst   float64
	Last    float64
	Samples int
}

type HostPeak struct {
	HostID              string
	MaxSandboxes        int
	SlotLimit           int
	PeakRunning         int
	PeakProvisioning    int
	PeakUsedSlots       int
	PeakAllocatedVCPUs  int64
	PeakAllocatedMemMiB int64
	MinHeadroomPct      float64
	FullObserved        bool
}

type PressureReport struct {
	Mode            string
	Sources         []string
	Evaluations     int
	SourceErrors    map[string]int
	NoData          map[string]int
	Backoffs        int
	MinMultiplier   float64
	PausedFor       time.Duration
	Aborted         string
	TriggerCounts   map[string]int
	Triggers        []GuardTrigger
	DroppedTriggers int
	Peaks           []SignalPeak
	Hosts           []HostPeak
	Unconfigured    []string
}

type createObservation struct {
	at       time.Time
	duration time.Duration
	failed   bool
}

// PressureMonitor polls pressure sources and turns threshold crossings into
// launch backoffs or an abort. All methods are safe on a nil monitor, which
// behaves as a disabled guard.
type PressureMonitor struct {
	cfg     PressureConfig
	rules   map[string]PressureRule
	hostIDs map[string]bool
	sources []PressureSource
	now     func() time.Time

	mu          sync.Mutex
	multiplier  float64
	minMult     float64
	pausedUntil time.Time
	pausedFor   time.Duration
	consecutive int
	backoffs    int
	abortErr    error
	abortCh     chan struct{}
	evaluations int
	srcErrors   map[string]int
	noData      map[string]int
	triggers    []GuardTrigger
	dropped     int
	counts      map[string]int
	peaks       map[string]*SignalPeak
	hosts       map[string]*HostPeak
	lastHosts   []HostPressure
	creates     []createObservation
}

var ErrGuardAbort = errors.New("host pressure guard aborted the launch")

func NewPressureMonitor(cfg PressureConfig, sources []PressureSource, now func() time.Time) *PressureMonitor {
	if cfg.Mode == PressureModeOff || cfg.Mode == "" {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	m := &PressureMonitor{
		cfg: cfg, sources: sources, now: now, rules: make(map[string]PressureRule),
		multiplier: 1, minMult: 1, abortCh: make(chan struct{}), srcErrors: make(map[string]int), noData: make(map[string]int),
		counts: make(map[string]int), peaks: make(map[string]*SignalPeak), hosts: make(map[string]*HostPeak),
	}
	for _, rule := range cfg.Rules {
		m.rules[rule.Signal] = rule
	}
	if len(cfg.HostIDs) > 0 {
		m.hostIDs = make(map[string]bool, len(cfg.HostIDs))
		for _, id := range cfg.HostIDs {
			m.hostIDs[id] = true
		}
	}
	return m
}

// Run evaluates the guard every interval until ctx ends or the guard aborts.
// The first evaluation happens one interval after the call, so callers
// evaluate once synchronously before they start launching.
func (m *PressureMonitor) Run(ctx context.Context) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.abortCh:
			return
		case <-ticker.C:
		}
		m.Evaluate(ctx)
	}
}

// Evaluate reads every source once and applies the resulting action. It
// returns the action an enforcing guard took, or "" when it took none.
func (m *PressureMonitor) Evaluate(ctx context.Context) string {
	if m == nil {
		return ""
	}
	var samples []PressureSample
	var hosts []HostPressure
	var unavailable, noData []string
	for _, source := range m.sources {
		sourceCtx, cancel := context.WithTimeout(ctx, m.cfg.SourceTimeout)
		reading, err := source.Read(sourceCtx)
		cancel()
		if ctx.Err() != nil {
			return ""
		}
		for _, sample := range reading.Samples {
			if m.watchesHost(sample.HostID) {
				samples = append(samples, sample)
			}
		}
		for _, host := range reading.Hosts {
			if m.watchesHost(host.HostID) {
				hosts = append(hosts, host)
			}
		}
		noData = append(noData, reading.NoData...)
		if err != nil {
			log.Warn().Err(err).Str("source", source.Name()).Msg("host pressure source unavailable")
			unavailable = append(unavailable, source.Name())
		}
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.abortErr != nil {
		return GuardActionAbort
	}
	m.evaluations++
	for _, signal := range noData {
		m.noData[signal]++
	}
	samples = append(samples, m.generatorSamplesLocked(now)...)
	if len(hosts) > 0 {
		m.recordHostsLocked(hosts)
	}
	action := ""
	for _, sample := range samples {
		rule, ok := m.rules[sample.Signal]
		if !ok {
			continue
		}
		m.recordPeakLocked(rule, sample)
		if crossed(rule.LowerIsWorse, sample.Value, rule.Abort) {
			m.triggerLocked(now, sample.Signal, sample.HostID, sample.Value, rule.Abort, GuardActionAbort)
			action = GuardActionAbort
		} else if crossed(rule.LowerIsWorse, sample.Value, rule.Backoff) {
			m.triggerLocked(now, sample.Signal, sample.HostID, sample.Value, rule.Backoff, GuardActionBackoff)
			if action == "" {
				action = GuardActionBackoff
			}
		}
	}
	// A guard that cannot see the hosts must not keep launching at full
	// rate: unavailable sources back off and eventually escalate.
	for _, name := range unavailable {
		m.srcErrors[name]++
		m.triggerLocked(now, "source_unavailable", name, 1, 0, GuardActionBackoff)
		if action == "" {
			action = GuardActionBackoff
		}
	}
	if m.cfg.Mode != PressureModeEnforce {
		return ""
	}
	switch action {
	case GuardActionAbort:
		m.abortLocked("threshold crossed")
	case GuardActionBackoff:
		m.consecutive++
		if m.consecutive > m.cfg.MaxBackoffs {
			m.triggerLocked(now, "backoff_escalation", "", float64(m.consecutive), float64(m.cfg.MaxBackoffs), GuardActionAbort)
			m.abortLocked(fmt.Sprintf("%d consecutive backoffs", m.consecutive))
			return GuardActionAbort
		}
		m.backoffs++
		m.multiplier = math.Max(m.multiplier/2, minRateMultiplier)
		m.minMult = math.Min(m.minMult, m.multiplier)
		if m.cfg.BackoffPause > 0 {
			m.pausedUntil = now.Add(m.cfg.BackoffPause)
			m.pausedFor += m.cfg.BackoffPause
		}
	default:
		m.consecutive = 0
		m.multiplier = math.Min(1, m.multiplier*2)
	}
	return action
}

// watchesHost keeps fleet-wide samples and, when LOAD_TEST_GUARD_HOST_IDS is
// set, only the listed hosts.
func (m *PressureMonitor) watchesHost(hostID string) bool {
	return hostID == "" || m.hostIDs == nil || m.hostIDs[hostID]
}

func crossed(lowerIsWorse bool, value, threshold float64) bool {
	if threshold <= 0 {
		return false
	}
	if lowerIsWorse {
		return value < threshold
	}
	return value > threshold
}

func (m *PressureMonitor) triggerLocked(at time.Time, signal, hostID string, value, threshold float64, action string) {
	trigger := GuardTrigger{At: at.UTC(), Signal: signal, HostID: hostID, Value: value, Threshold: threshold, Action: action, Enforced: m.cfg.Mode == PressureModeEnforce}
	m.counts[signal+"/"+action]++
	if len(m.triggers) < maxRecordedTriggers {
		m.triggers = append(m.triggers, trigger)
	} else {
		m.dropped++
	}
	log.Warn().Str("signal", signal).Str("host_id", hostID).Float64("value", value).Float64("threshold", threshold).
		Str("action", action).Bool("enforced", trigger.Enforced).Msg("host pressure guard triggered")
}

func (m *PressureMonitor) abortLocked(reason string) {
	m.abortErr = fmt.Errorf("%w: %s", ErrGuardAbort, reason)
	close(m.abortCh)
}

func (m *PressureMonitor) recordPeakLocked(rule PressureRule, sample PressureSample) {
	key := sample.Signal + "\x00" + sample.HostID
	peak, ok := m.peaks[key]
	if !ok {
		peak = &SignalPeak{Signal: sample.Signal, HostID: sample.HostID, Worst: sample.Value}
		m.peaks[key] = peak
	}
	if rule.LowerIsWorse && sample.Value < peak.Worst || !rule.LowerIsWorse && sample.Value > peak.Worst {
		peak.Worst = sample.Value
	}
	peak.Last = sample.Value
	peak.Samples++
}

func (m *PressureMonitor) recordHostsLocked(rows []HostPressure) {
	m.lastHosts = append(m.lastHosts[:0], rows...)
	for _, row := range rows {
		peak, ok := m.hosts[row.HostID]
		if !ok {
			peak = &HostPeak{HostID: row.HostID, MinHeadroomPct: -1}
			m.hosts[row.HostID] = peak
		}
		peak.MaxSandboxes, peak.SlotLimit = row.MaxSandboxes, row.SlotLimit()
		peak.PeakRunning = max(peak.PeakRunning, row.RunningSandboxes)
		peak.PeakProvisioning = max(peak.PeakProvisioning, row.ProvisioningSandboxes)
		peak.PeakUsedSlots = max(peak.PeakUsedSlots, row.UsedNetSlots+row.ProvisioningNetSlots)
		peak.PeakAllocatedVCPUs = max(peak.PeakAllocatedVCPUs, row.AllocatedVCPUs)
		peak.PeakAllocatedMemMiB = max(peak.PeakAllocatedMemMiB, row.AllocatedMemoryMiB)
		if headroom := row.HeadroomPct(); headroom >= 0 && (peak.MinHeadroomPct < 0 || headroom < peak.MinHeadroomPct) {
			peak.MinHeadroomPct = headroom
		}
		peak.FullObserved = peak.FullObserved || row.Full()
	}
}

func (m *PressureMonitor) generatorSamplesLocked(now time.Time) []PressureSample {
	cutoff := now.Add(-m.cfg.CreateErrorWindow)
	kept := m.creates[:0]
	for _, obs := range m.creates {
		if !obs.at.Before(cutoff) {
			kept = append(kept, obs)
		}
	}
	m.creates = kept
	if len(kept) < m.cfg.CreateErrorMinSamples {
		return nil
	}
	failures := 0
	durations := make([]time.Duration, 0, len(kept))
	for _, obs := range kept {
		if obs.failed {
			failures++
		} else {
			durations = append(durations, obs.duration)
		}
	}
	samples := []PressureSample{{Signal: "create_error_ratio", Value: float64(failures) / float64(len(kept))}}
	if len(durations) > 0 {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		samples = append(samples, PressureSample{Signal: "create_p99_seconds", Value: nearestRank(durations, 99).Seconds()})
	}
	return samples
}

// ObserveCreate feeds one create request outcome into the generator signals.
func (m *PressureMonitor) ObserveCreate(at time.Time, duration time.Duration, failed bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.creates = append(m.creates, createObservation{at: at, duration: duration, failed: failed})
	m.mu.Unlock()
}

// Gate blocks while a backoff pause is active and returns the current rate
// multiplier. paused reports whether the caller waited, so a pacer can reset
// its schedule instead of bursting to catch up.
func (m *PressureMonitor) Gate(ctx context.Context) (multiplier float64, paused bool, err error) {
	if m == nil {
		return 1, false, nil
	}
	for {
		m.mu.Lock()
		if m.abortErr != nil {
			err := m.abortErr
			m.mu.Unlock()
			return 0, paused, err
		}
		wait := m.pausedUntil.Sub(m.now())
		multiplier = m.multiplier
		m.mu.Unlock()
		if wait <= 0 {
			return multiplier, paused, nil
		}
		paused = true
		timer := time.NewTimer(min(wait, time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, paused, ctx.Err()
		case <-m.abortCh:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (m *PressureMonitor) Err() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.abortErr
}

// AllHostsFull reports whether the latest host_pressure snapshot shows every
// host at a published cap.
func (m *PressureMonitor) AllHostsFull() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.lastHosts) == 0 {
		return false
	}
	for _, host := range m.lastHosts {
		if !host.Full() {
			return false
		}
	}
	return true
}

func (m *PressureMonitor) Report() PressureReport {
	if m == nil {
		return PressureReport{Mode: PressureModeOff}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	report := PressureReport{
		Mode: m.cfg.Mode, Evaluations: m.evaluations, Backoffs: m.backoffs, MinMultiplier: m.minMult, PausedFor: m.pausedFor,
		SourceErrors: maps.Clone(m.srcErrors), NoData: maps.Clone(m.noData), TriggerCounts: maps.Clone(m.counts),
		Triggers: append([]GuardTrigger(nil), m.triggers...), DroppedTriggers: m.dropped,
	}
	if m.abortErr != nil {
		report.Aborted = m.abortErr.Error()
	}
	for _, source := range m.sources {
		report.Sources = append(report.Sources, source.Name())
	}
	report.Sources = append(report.Sources, PressureSourceGenerator)
	for _, peak := range m.peaks {
		report.Peaks = append(report.Peaks, *peak)
	}
	sort.Slice(report.Peaks, func(i, j int) bool {
		if report.Peaks[i].Signal != report.Peaks[j].Signal {
			return report.Peaks[i].Signal < report.Peaks[j].Signal
		}
		return report.Peaks[i].HostID < report.Peaks[j].HostID
	})
	for _, host := range m.hosts {
		report.Hosts = append(report.Hosts, *host)
	}
	sort.Slice(report.Hosts, func(i, j int) bool { return report.Hosts[i].HostID < report.Hosts[j].HostID })
	for _, rule := range m.cfg.Rules {
		if !slices.Contains(report.Sources, rule.Source) || rule.Source == PressureSourceGMP && rule.Query == "" {
			report.Unconfigured = append(report.Unconfigured, rule.Signal)
		}
	}
	return report
}
