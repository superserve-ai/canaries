package loadrunner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReportUsesUntrimmedNearestRankWithoutMutatingSamples(t *testing.T) {
	// Retain the entire population, including the one-hour outlier, when
	// selecting rank 99. Trimming either end would change these ranks.
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(100-i) * time.Millisecond
	}
	samples[0] = time.Hour
	before := append([]time.Duration(nil), samples...)
	summary := Summary{Started: 100, Completed: 100, Duration: 2 * time.Second, Create: PhaseSummary{Attempts: 100, Durations: samples}, LifecycleDurations: []time.Duration{time.Second}}
	report := summary.Report(Config{Environment: "staging", Region: "us-central1", Target: "staging-us-central1", RunID: "test-run", Operations: 100, Concurrency: 10}, nil)
	row := reportRow(t, report, "Create")
	if want := []string{"Create", "100", "50.000", "95.000", "99.000"}; !reflect.DeepEqual(row, want) {
		t.Fatalf("Create row = %v, want %v", row, want)
	}
	if !reflect.DeepEqual(samples, before) {
		t.Fatal("Report reordered retained samples")
	}
	for _, want := range []string{"*Load Testing Results*", "Environment: staging | Region: us-central1 | Target: staging-us-central1", "Run ID: test-run", "Concurrency: 10 configured, 0 peak", "100 total, 100 started, 100 succeeded, 0 failed, 0 unstarted", "Wall time: 2.000 s", "Success rate: 100.00% of started", "50.000 lifecycles/s", "p50(ms)", "p95(ms)", "p99(ms)", "successful phase completions only"} {
		if !strings.Contains(report, want) {
			t.Fatalf("Report missing %q:\n%s", want, report)
		}
	}
}

func TestNearestRankRetainsSmallPopulationExtremes(t *testing.T) {
	samples := []time.Duration{time.Millisecond, 2 * time.Millisecond, time.Hour}
	for _, tt := range []struct {
		percentile int
		want       time.Duration
	}{{50, 2 * time.Millisecond}, {95, time.Hour}, {99, time.Hour}} {
		if got := nearestRank(samples, tt.percentile); got != tt.want {
			t.Errorf("p%d = %s, want %s", tt.percentile, got, tt.want)
		}
	}
}

func TestReportNoSamplesUsesDashes(t *testing.T) {
	report := (Summary{}).Report(Config{Operations: 100, Concurrency: 10}, errors.New("failure containing private-api-token"))
	for _, name := range []string{"Create", "Pause", "Resume", "Delete", "Lifecycle"} {
		if got, want := reportRow(t, report, name), []string{name, "0", "-", "-", "-"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s row = %v, want %v", name, got, want)
		}
	}
	for _, want := range []string{"Outcome: failed", "Success rate: - of started", "Successful lifecycle throughput: -", "Failure phases: none | Cleanup failures: 0"} {
		if !strings.Contains(report, want) {
			t.Errorf("Report missing %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, "Delete completion latency is not sampled in create mode.") {
		t.Fatal("Report omitted the create-only delete sampling caveat")
	}
	if strings.Contains(report, "private-api-token") || strings.Contains(report, "NaN") || strings.Contains(report, "Inf") {
		t.Fatalf("Report contains secret or invalid ratio:\n%s", report)
	}
}

func TestReportLifecycleModeDoesNotShowCreateOnlyCaveat(t *testing.T) {
	report := (Summary{}).Report(Config{Scenario: "lifecycle"}, nil)
	if strings.Contains(report, "not sampled in create mode") {
		t.Fatal("Report showed create-only caveat for a full lifecycle")
	}
}

func TestReportFailuresHaveExplicitCountsAndStableOrdering(t *testing.T) {
	summary := Summary{
		ConfiguredOperations: 100, ConfiguredConcurrency: 10, PeakInFlight: 9,
		Started: 20, Completed: 15, Failed: 5, Unstarted: 80, Duration: 10 * time.Second,
		CleanupFailures: 2,
		FailurePhases:   map[string]int64{"wait_paused": 3, "delete_completion": 2, "never_attempted": 0},
	}
	report := summary.Report(Config{}, context.Canceled)
	for _, want := range []string{"Outcome: canceled", "Concurrency: 10 configured, 9 peak", "100 total, 20 started, 15 succeeded, 5 failed, 80 unstarted", "Success rate: 75.00% of started", "1.500 lifecycles/s", "Failure phases: delete_completion=2, wait_paused=3 | Cleanup failures: 2"} {
		if !strings.Contains(report, want) {
			t.Errorf("Report missing %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "never_attempted") {
		t.Fatal("Report included a phase with zero failures")
	}
}

func reportRow(t *testing.T, report, name string) []string {
	t.Helper()
	for _, line := range strings.Split(report, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 5 && fields[0] == name {
			return fields
		}
	}
	t.Fatalf("missing %s latency row in report:\n%s", name, report)
	return nil
}
