package loadrunner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	maxReportTimelineRows = 60
	maxReportTriggers     = 25
	saturationLag         = time.Second
)

// LaunchReport renders the mass-launch summary. Platform-side load (restore
// rate per host, API and DB) comes from the guard's GMP samples when
// configured, and the run window lets readers line up dashboards.
func (s LaunchSummary) Report(cfg Config, runErr error) string {
	var out strings.Builder
	fmt.Fprintln(&out, "*Mass Launch Results*")
	fmt.Fprintf(&out, "Environment: %s | Region: %s | Target: %s\n", cfg.Environment, cfg.Region, cfg.Target)
	fmt.Fprintf(&out, "Run ID: %s | Worker: %s | Task: %d of %d\n", cfg.RunID, cfg.WorkerID, cfg.Launch.TaskIndex+1, max(cfg.Launch.TaskCount, 1))
	fmt.Fprintf(&out, "Outcome: %s", launchOutcome(s, runErr))
	if s.StopReason != "" {
		fmt.Fprintf(&out, " (%s)", s.StopReason)
	}
	fmt.Fprintln(&out)
	if !s.Start.IsZero() {
		fmt.Fprintf(&out, "Window (UTC): start %s | launch end %s | end %s\n", s.Start.Format(time.RFC3339), s.LaunchEnd.Format(time.RFC3339), s.End.Format(time.RFC3339))
	}

	t := s.Template
	if t.Known {
		fmt.Fprintf(&out, "Template: %s | %d vCPU | %d MiB memory | %d MiB disk | %s image\n", t.Name, t.VCPU, t.MemoryMiB, t.DiskMiB, formatBytes(t.SizeBytes))
	} else {
		fmt.Fprintf(&out, "Template: %s | size unknown (lookup failed or template not visible)\n", t.Name)
	}
	fmt.Fprintf(&out, "Strategy: %s | Target rate: %s | API budget: %.1f req/s aggregate | Concurrency: %d configured, %d peak | Hold: %s\n",
		cfg.Launch.Strategy, describeLaunchRate(cfg.Launch), cfg.Launch.APIRate, s.ConfiguredConcurrency, s.PeakInFlight, s.HoldDuration.Round(time.Second))
	fmt.Fprintf(&out, "Sandboxes: %d total, %d this task, %d dispatched, %d created, %d active, %d ready, %d failed, %d unstarted, %d unknown create outcome\n",
		s.Configured, s.TaskOperations, s.Dispatched, s.Created, s.Active, s.Ready, s.Failed, s.Unstarted, s.UncertainCreates)

	launchWindow := s.LaunchEnd.Sub(s.Start)
	fmt.Fprintf(&out, "Creates/s: %s avg, %d peak 1s, %s peak 10s | Ready/s: %s avg, %s peak 10s\n",
		perSecond(s.Created, launchWindow), peakWindow(s.Timeline, 1, acceptedOf), peakRate(s.Timeline, 10, acceptedOf),
		perSecond(s.Ready, launchWindow), peakRate(s.Timeline, 10, readyOf))

	fmt.Fprintln(&out, "```\nLatency                 n    p50(ms)    p95(ms)    p99(ms)")
	for _, row := range []struct {
		name    string
		samples []time.Duration
	}{
		{"Create request", s.CreateRequest},
		{"Create -> active", s.CreateToActive},
		{"Create -> 1st command", s.TimeToFirstCommand},
	} {
		p50, p95, p99 := percentiles(row.samples)
		fmt.Fprintf(&out, "%-21s %5d %10s %10s %10s\n", row.name, len(row.samples), p50, p95, p99)
	}
	fmt.Fprintln(&out, "```")

	fmt.Fprintf(&out, "Failures by class: %s\n", formatCounts(s.FailureClasses))
	scaleLabel := "Failures by scale"
	if cfg.Launch.TaskCount > 1 {
		scaleLabel += " (this task's failures by global operation index)"
	}
	fmt.Fprintf(&out, "%s: %s\n", scaleLabel, formatMilestones(s.FailureMilestones, s.Dispatched, s.Failed, s.Dispatched*int64(max(cfg.Launch.TaskCount, 1))))
	writeTimeline(&out, s.Timeline)
	writeGeneratorSection(&out, s, cfg)
	writePressureSection(&out, s.Pressure)

	c := s.Cleanup
	fmt.Fprintf(&out, "Cleanup: %d resident, %d deleted, %d swept, %d delete failures, %d leftover, verified=%t", c.Resident, c.Deleted, c.Swept, c.DeleteFailures, c.Leftover, c.Verified)
	if c.Note != "" {
		fmt.Fprintf(&out, " (%s)", c.Note)
	}
	fmt.Fprintln(&out)
	if !s.Start.IsZero() {
		environment := cfg.Pressure.GMPEnvironment
		if environment == "" {
			environment = cfg.Environment
		}
		fmt.Fprintf(&out, "Correlate: query GMP for environment=%q from %s to %s: sum by (host_id) (rate(vmd_call_total{method=\"CreateVM\"}[1m])), sandbox_transition_total, db_pool_acquired_conns / db_pool_total_conns.",
			environment, s.Start.Format(time.RFC3339), s.End.Format(time.RFC3339))
	}
	return out.String()
}

// Totals maps the launch summary onto the shared run summary used by the
// load runner's terminal log line.
func (s LaunchSummary) Totals() Summary {
	return Summary{
		Started: s.Dispatched, Completed: s.Ready, Failed: s.Failed, ConfiguredOperations: s.TaskOperations,
		ConfiguredConcurrency: s.ConfiguredConcurrency, PeakInFlight: s.PeakInFlight, Unstarted: s.Unstarted,
		Duration: s.End.Sub(s.Start), CleanupFailures: s.Cleanup.DeleteFailures + s.Cleanup.Leftover,
	}
}

func launchOutcome(s LaunchSummary, runErr error) string {
	switch {
	case errors.Is(runErr, ErrGuardAbort):
		return "aborted by host pressure guard"
	case errors.Is(runErr, context.Canceled):
		return "canceled"
	case errors.Is(runErr, context.DeadlineExceeded):
		return "timed out"
	case runErr != nil || s.Failed > 0:
		return "failed"
	}
	return "succeeded"
}

func describeLaunchRate(l LaunchConfig) string {
	switch {
	case l.Strategy == "ramp":
		return fmt.Sprintf("%.1f/s rising by %.1f/s every %s to %.1f/s", l.RampStartRate, l.RampStepRate, l.RampStepDuration, l.Rate)
	case l.Rate > 0:
		return fmt.Sprintf("%.1f/s", l.Rate)
	}
	return "unpaced (concurrency bound)"
}

// writeGeneratorSection separates generator saturation from platform
// saturation. The generator is the bottleneck when creates wait for a local
// slot or fall behind schedule while the API answers without throttling.
func writeGeneratorSection(out *strings.Builder, s LaunchSummary, cfg Config) {
	lagP50, _, lagP99 := percentiles(s.ScheduleLag)
	_, _, slotP99 := percentiles(s.SlotWait)
	fmt.Fprintf(out, "Generator: schedule lag p50 %s ms, p99 %s ms | slot wait p99 %s ms | peak in-flight %d of %d | API budget wait %.1f s over %d calls",
		lagP50, lagP99, slotP99, s.PeakInFlight, s.ConfiguredConcurrency, s.APIWait.Seconds(), s.APIWaitCalls)
	if taskRate := cfg.Launch.TaskRate(cfg.Launch.Rate); taskRate > 0 && len(s.TimeToFirstCommand) > 0 {
		if ttiP50, _, _ := percentileDurations(s.TimeToFirstCommand); ttiP50 > 0 {
			fmt.Fprintf(out, " | concurrency needed for target rate ~%.0f", taskRate*ttiP50.Seconds())
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Bottleneck: %s\n", strings.Join(launchBottlenecks(s, cfg), "; "))
}

func launchBottlenecks(s LaunchSummary, cfg Config) []string {
	var reasons []string
	launchWindow := s.LaunchEnd.Sub(s.Start).Seconds()
	achieved := 0.0
	if launchWindow > 0 {
		achieved = float64(s.Created) / launchWindow
	}
	taskAPIRate := cfg.Launch.TaskRate(cfg.Launch.APIRate)
	if n := s.FailureClasses[FailureRateLimited]; n > 0 {
		reasons = append(reasons, fmt.Sprintf("API rate limit (%d create 429s); use more teams or a rate-limit exemption", n))
	}
	if n := s.FailureClasses[FailureTeamQuota]; n > 0 {
		reasons = append(reasons, fmt.Sprintf("team sandbox quota (%d rejections)", n))
	}
	if n := s.FailureClasses[FailureHostCap]; n > 0 {
		reasons = append(reasons, fmt.Sprintf("host caps (%d rejections; see host caps)", n))
	}
	if s.Pressure.Backoffs > 0 || s.Pressure.Aborted != "" {
		reasons = append(reasons, fmt.Sprintf("host pressure guard (%d backoffs)", s.Pressure.Backoffs))
	}
	if n := s.FailureClasses[FailureServerError] + s.FailureClasses[FailureActiveFailed] + s.FailureClasses[FailureActiveWait] + s.FailureClasses[FailureFirstCommand]; n > 0 {
		reasons = append(reasons, fmt.Sprintf("platform errors (%d)", n))
	}
	if taskAPIRate > 0 && achieved >= 0.8*taskAPIRate {
		reasons = append(reasons, fmt.Sprintf("generator API budget (%.1f creates/s against a %.1f req/s task budget)", achieved, taskAPIRate))
	}
	if len(s.SlotWait) > 0 {
		if _, _, p99 := percentileDurations(s.SlotWait); p99 > saturationLag {
			reasons = append(reasons, "generator concurrency (creates waited for a local slot; raise LOAD_TEST_CONCURRENCY or task count)")
		}
	}
	if len(s.ScheduleLag) > 0 {
		if _, _, p99 := percentileDurations(s.ScheduleLag); p99 > saturationLag {
			reasons = append(reasons, "generator fell behind its schedule")
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "none detected at this rate")
	}
	return reasons
}

func writePressureSection(out *strings.Builder, p PressureReport) {
	if p.Mode == "" || p.Mode == PressureModeOff {
		fmt.Fprintln(out, "Host pressure guard: off")
		return
	}
	fmt.Fprintf(out, "Host pressure guard: mode=%s | sources=%s | evaluations=%d | backoffs=%d | min rate multiplier=%.3f | paused=%s",
		p.Mode, strings.Join(p.Sources, ","), p.Evaluations, p.Backoffs, p.MinMultiplier, p.PausedFor)
	if p.Aborted != "" {
		fmt.Fprintf(out, " | aborted: %s", p.Aborted)
	}
	fmt.Fprintln(out)
	if len(p.SourceErrors) > 0 {
		fmt.Fprintf(out, "Guard source errors: %s\n", formatCounts(p.SourceErrors))
	}
	if len(p.NoData) > 0 {
		fmt.Fprintf(out, "Guard signals without data (evaluations): %s\n", formatCounts(p.NoData))
	}
	if len(p.Unconfigured) > 0 {
		fmt.Fprintf(out, "Guard signals not read: %s\n", strings.Join(p.Unconfigured, ", "))
	}
	if len(p.Triggers) == 0 {
		fmt.Fprintln(out, "Guard triggers: none")
	} else {
		fmt.Fprintf(out, "Guard triggers (%d recorded, %d not recorded): %s\n", len(p.Triggers), p.DroppedTriggers, formatCounts(p.TriggerCounts))
		fmt.Fprintln(out, "```")
		for i, trigger := range p.Triggers {
			if i == maxReportTriggers {
				fmt.Fprintf(out, "... %d more in logs\n", len(p.Triggers)-maxReportTriggers)
				break
			}
			host := trigger.HostID
			if host == "" {
				host = "-"
			}
			enforced := "enforced"
			if !trigger.Enforced {
				enforced = "observed"
			}
			fmt.Fprintf(out, "%s %-28s %-24s value=%.3f threshold=%.3f %s %s\n", trigger.At.Format("15:04:05"), trigger.Signal, host, trigger.Value, trigger.Threshold, trigger.Action, enforced)
		}
		fmt.Fprintln(out, "```")
	}
	if len(p.Hosts) > 0 {
		fmt.Fprintln(out, "Host caps (host_pressure):\n```\nhost                      max_sbx slot_limit peak_run peak_prov peak_slots peak_vcpu peak_mem_mib min_headroom full")
		for _, h := range p.Hosts {
			headroom := "-"
			if h.MinHeadroomPct >= 0 {
				headroom = fmt.Sprintf("%.1f%%", h.MinHeadroomPct)
			}
			fmt.Fprintf(out, "%-25s %7s %10s %8d %9d %10d %9d %12d %12s %t\n", h.HostID, capValue(h.MaxSandboxes), capValue(h.SlotLimit),
				h.PeakRunning, h.PeakProvisioning, h.PeakUsedSlots, h.PeakAllocatedVCPUs, h.PeakAllocatedMemMiB, headroom, h.FullObserved)
		}
		fmt.Fprintln(out, "```")
	} else {
		fmt.Fprintln(out, "Host caps: not read (no host_pressure source)")
	}
	if len(p.Peaks) > 0 {
		fmt.Fprintln(out, "Signal peaks (worst / last):\n```")
		for _, peak := range p.Peaks {
			host := peak.HostID
			if host == "" {
				host = "-"
			}
			fmt.Fprintf(out, "%-30s %-24s %12.3f %12.3f n=%d\n", peak.Signal, host, peak.Worst, peak.Last, peak.Samples)
		}
		fmt.Fprintln(out, "```")
	}
}

func writeTimeline(out *strings.Builder, timeline []LaunchSecond) {
	if len(timeline) == 0 {
		fmt.Fprintln(out, "Timeline: no completions")
		return
	}
	width := 10
	if rows := (len(timeline) + width - 1) / width; rows > maxReportTimelineRows {
		width = (len(timeline) + maxReportTimelineRows - 1) / maxReportTimelineRows
	}
	fmt.Fprintf(out, "Timeline (%ds buckets, per second):\n```\n   t+s  creates/s  ready/s  failed/s\n", width)
	for start := 0; start < len(timeline); start += width {
		var bucket LaunchSecond
		end := min(start+width, len(timeline))
		for _, second := range timeline[start:end] {
			bucket.Accepted += second.Accepted
			bucket.Ready += second.Ready
			bucket.Failed += second.Failed
		}
		span := float64(end - start)
		fmt.Fprintf(out, "%6d %10.1f %8.1f %9.1f\n", start, float64(bucket.Accepted)/span, float64(bucket.Ready)/span, float64(bucket.Failed)/span)
	}
	fmt.Fprintln(out, "```")
}

func acceptedOf(s LaunchSecond) int64 { return s.Accepted }
func readyOf(s LaunchSecond) int64    { return s.Ready }

func peakWindow(timeline []LaunchSecond, width int, value func(LaunchSecond) int64) int64 {
	var peak, sum int64
	for i, second := range timeline {
		sum += value(second)
		if i >= width {
			sum -= value(timeline[i-width])
		}
		peak = max(peak, sum)
	}
	return peak
}

func peakRate(timeline []LaunchSecond, width int, value func(LaunchSecond) int64) string {
	if len(timeline) == 0 {
		return "-"
	}
	span := min(width, len(timeline))
	return fmt.Sprintf("%.1f", float64(peakWindow(timeline, span, value))/float64(span))
}

func perSecond(count int64, window time.Duration) string {
	if window <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", float64(count)/window.Seconds())
}

func percentileDurations(samples []time.Duration) (p50, p95, p99 time.Duration) {
	ordered := slices.Sorted(slices.Values(samples))
	return nearestRank(ordered, 50), nearestRank(ordered, 95), nearestRank(ordered, 99)
}

func percentiles(samples []time.Duration) (p50, p95, p99 string) {
	if len(samples) == 0 {
		return "-", "-", "-"
	}
	a, b, c := percentileDurations(samples)
	return milliseconds(a), milliseconds(b), milliseconds(c)
}

func formatCounts[V int | int64](counts map[string]V) string {
	keys := make([]string, 0, len(counts))
	for key, count := range counts {
		if count > 0 {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return "none"
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ", ")
}

// formatMilestones buckets failures by global operation index. scale is the
// number of global operations this task's share spans, so a fan-out task
// reports every bucket its failures fall into.
func formatMilestones(milestones []FailureMilestone, dispatched, failed, scale int64) string {
	if dispatched == 0 {
		return "-"
	}
	failedBefore := make(map[int64]int64, len(milestones))
	for _, m := range milestones {
		failedBefore[m.Operations] = m.Failed
	}
	var parts []string
	var cumulative int64
	for _, milestone := range failureMilestones {
		cumulative += failedBefore[milestone]
		if milestone >= scale {
			parts = append(parts, fmt.Sprintf("all %d: %d", dispatched, failed))
			break
		}
		parts = append(parts, fmt.Sprintf("first %d: %d", milestone, cumulative))
	}
	return strings.Join(parts, " | ")
}

func capValue(value int) string {
	if value <= 0 {
		return "unset"
	}
	return fmt.Sprint(value)
}

func formatBytes(n int64) string {
	if n <= 0 {
		return "unknown-size"
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}
