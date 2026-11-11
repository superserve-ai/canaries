package loadrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Report summarizes the observations retained by Run without making API calls.
func (s Summary) Report(cfg Config, runErr error) string {
	if s.Resident != nil {
		data, _ := json.Marshal(s.Resident)
		if s.Resident.SharedHost {
			return "Shared-host staging resident result (does not qualify 105000-owner scale; external host/database evidence required): " + string(data)
		}
		return "Resident paused-fleet scenario result (external host/database evidence required): " + string(data)
	}
	var out strings.Builder
	fmt.Fprintln(&out, "*Load Testing Results*")
	fmt.Fprintf(&out, "Environment: %s | Region: %s | Target: %s\n", cfg.Environment, cfg.Region, cfg.Target)
	fmt.Fprintf(&out, "Run ID: %s\n", cfg.RunID)
	outcome := "succeeded"
	switch {
	case errors.Is(runErr, context.Canceled):
		outcome = "canceled"
	case errors.Is(runErr, context.DeadlineExceeded):
		outcome = "timed out"
	case runErr != nil || s.Failed > 0:
		outcome = "failed"
	}
	fmt.Fprintf(&out, "Outcome: %s\n", outcome)
	configuredTotal, configuredConcurrency := s.ConfiguredOperations, s.ConfiguredConcurrency
	if configuredTotal == 0 {
		configuredTotal = int64(cfg.Operations)
	}
	if configuredConcurrency == 0 {
		configuredConcurrency = int64(cfg.Concurrency)
	}
	fmt.Fprintf(&out, "Concurrency: %d configured, %d peak\n", configuredConcurrency, s.PeakInFlight)
	fmt.Fprintf(&out, "Sandboxes: %d total, %d started, %d succeeded, %d failed, %d unstarted\n", configuredTotal, s.Started, s.Completed, s.Failed, s.Unstarted)
	fmt.Fprintf(&out, "Wall time: %.3f s\n", s.Duration.Seconds())
	successRate := "-"
	if s.Started > 0 {
		successRate = fmt.Sprintf("%.2f%%", 100*float64(s.Completed)/float64(s.Started))
	}
	throughput := "-"
	if s.Duration > 0 {
		throughput = fmt.Sprintf("%.3f lifecycles/s", float64(s.Completed)/s.Duration.Seconds())
	}
	fmt.Fprintf(&out, "Success rate: %s of started | Successful lifecycle throughput: %s\n", successRate, throughput)
	fmt.Fprintln(&out, "```\nPhase          n    p50(ms)    p95(ms)    p99(ms)")
	for _, phase := range []struct {
		name      string
		durations []time.Duration
	}{
		{"Create", s.Create.Durations},
		{"Pause", s.Pause.Durations},
		{"Resume", s.Resume.Durations},
		{"Delete", s.Delete.Durations},
		{"Lifecycle", s.LifecycleDurations},
	} {
		p50, p95, p99 := "-", "-", "-"
		if len(phase.durations) > 0 {
			ordered := append([]time.Duration(nil), phase.durations...)
			sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
			p50 = milliseconds(nearestRank(ordered, 50))
			p95 = milliseconds(nearestRank(ordered, 95))
			p99 = milliseconds(nearestRank(ordered, 99))
		}
		fmt.Fprintf(&out, "%-10s %5d %10s %10s %10s\n", phase.name, len(phase.durations), p50, p95, p99)
	}
	fmt.Fprintln(&out, "```")
	fmt.Fprintln(&out, "Latency samples include successful phase completions only; all times are milliseconds.")
	if cfg.Scenario == "" || cfg.Scenario == "create" {
		fmt.Fprintln(&out, "Delete completion latency is not sampled in create mode.")
	}
	failurePhases := make([]string, 0, len(s.FailurePhases))
	for phase, count := range s.FailurePhases {
		if count > 0 {
			failurePhases = append(failurePhases, phase)
		}
	}
	sort.Strings(failurePhases)
	if len(failurePhases) == 0 {
		fmt.Fprintf(&out, "Failure phases: none | Cleanup failures: %d", s.CleanupFailures)
	} else {
		counts := make([]string, 0, len(failurePhases))
		for _, phase := range failurePhases {
			counts = append(counts, fmt.Sprintf("%s=%d", phase, s.FailurePhases[phase]))
		}
		fmt.Fprintf(&out, "Failure phases: %s | Cleanup failures: %d", strings.Join(counts, ", "), s.CleanupFailures)
	}
	return out.String()
}

// nearestRank expects a nonempty sorted population and an integer percentile.
func nearestRank(ordered []time.Duration, percentile int) time.Duration {
	return ordered[(len(ordered)*percentile+99)/100-1]
}

func milliseconds(duration time.Duration) string {
	return fmt.Sprintf("%.3f", float64(duration)/float64(time.Millisecond))
}
