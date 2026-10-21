package lifecycle

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
)

// Burst mirrors the public burst time-to-interactive benchmark: every sandbox
// is created at once, and TTI runs from the create request until the first
// command succeeds. Delete is not timed and starts as soon as a sandbox is done.
const (
	burstTemplate       = "superserve/node-22"
	burstCreateTimeout  = 120 * time.Second
	burstCommandTimeout = 30 * time.Second
)

type burstSample struct {
	TTI     time.Duration
	ReadyAt time.Duration
	Err     error
}

func (r Runner) runBurst(ctx context.Context, runID string) (res RunResult) {
	n := r.Config.BurstSize
	samples := make([]burstSample, n)
	start := r.Clock()

	var wg sync.WaitGroup
	for i := range samples {
		wg.Add(1)
		go func() {
			defer wg.Done()
			samples[i] = r.burstOne(ctx, runID, i, start)
		}()
	}
	wg.Wait()

	summary := summarizeBurst(samples)
	res.Report = summary.report(r.Config.Target)
	if summary.Succeeded < n {
		res.Err = fmt.Errorf("%d of %d burst sandboxes failed: %s", n-summary.Succeeded, n, summary.FirstError)
		res.FailedStep = "burst"
	}
	log.Info().
		Int("sandboxes", n).
		Int("succeeded", summary.Succeeded).
		Float64("score", summary.Score).
		Int64("tti_median_ms", summary.Median.Milliseconds()).
		Int64("tti_p95_ms", summary.P95.Milliseconds()).
		Int64("tti_p99_ms", summary.P99.Milliseconds()).
		Int64("wall_clock_ms", summary.WallClock.Milliseconds()).
		Int64("first_ready_ms", summary.FirstReady.Milliseconds()).
		Msg("burst benchmark summary")
	return res
}

func (r Runner) burstOne(ctx context.Context, runID string, index int, burstStart time.Time) (sample burstSample) {
	ops := r.operations()
	telemetry := r.telemetry()
	resources := RunResources{RunID: runID, CreatedAt: r.Clock().UTC()}
	req := r.canaryCreateSandboxRequest(resources)
	req.Name = sandboxName(r.Config.Target, fmt.Sprintf("%d-%s", index, runID))
	req.FromTemplate = burstTemplate

	start := r.Clock()
	createCtx, cancel := context.WithTimeout(ctx, burstCreateTimeout)
	sb, err := r.Client.CreateSandbox(createCtx, req)
	cancel()
	if err != nil {
		sample.Err = fmt.Errorf("create: %w", err)
		ops.RecordStep(ctx, telemetry, "tti", "failure", r.Clock().Sub(start))
		return sample
	}
	defer func() {
		if err := ops.DeleteSandboxBestEffort(context.Background(), sb.ID, DeleteSandboxOptions{Timeout: r.Config.DeleteTimeout, Telemetry: telemetry}); err != nil {
			log.Warn().Err(err).Str("sandbox_id", sb.ID).Msg("burst sandbox delete failed")
		}
	}()

	execCtx, cancel := context.WithTimeout(ctx, burstCommandTimeout)
	out, err := r.Client.Exec(execCtx, sb.ID, sb.AccessToken, canaryapi.ExecRequest{Command: "node -v", TimeoutS: int(burstCommandTimeout.Seconds())})
	cancel()
	if err == nil && out.ExitCode != 0 {
		err = ExecValidationError{ExitCode: out.ExitCode, Stderr: out.Stderr}
	}
	end := r.Clock()
	if err != nil {
		sample.Err = fmt.Errorf("node -v on %s: %w", sb.ID, err)
		ops.RecordStep(ctx, telemetry, "tti", "failure", end.Sub(start))
		return sample
	}
	sample.TTI = end.Sub(start)
	sample.ReadyAt = end.Sub(burstStart)
	ops.RecordStep(ctx, telemetry, "tti", "success", sample.TTI)
	return sample
}

type burstSummary struct {
	Total, Succeeded int
	Median, P95, P99 time.Duration
	WallClock        time.Duration
	FirstReady       time.Duration
	Score            float64
	FirstError       string
}

func summarizeBurst(samples []burstSample) burstSummary {
	s := burstSummary{Total: len(samples)}
	var ttis []float64
	for _, sample := range samples {
		if sample.Err != nil {
			if s.FirstError == "" {
				s.FirstError = sample.Err.Error()
			}
			continue
		}
		s.Succeeded++
		ttis = append(ttis, float64(sample.TTI)/float64(time.Millisecond))
		if sample.ReadyAt > s.WallClock {
			s.WallClock = sample.ReadyAt
		}
		if s.FirstReady == 0 || sample.ReadyAt < s.FirstReady {
			s.FirstReady = sample.ReadyAt
		}
	}
	if s.Succeeded == 0 {
		return s
	}
	median, p95, p99 := trimmedStats(ttis)
	s.Median, s.P95, s.P99 = msDuration(median), msDuration(p95), msDuration(p99)
	timing := 0.60*metricScore(median) + 0.25*metricScore(p95) + 0.15*metricScore(p99)
	successRate := float64(s.Succeeded) / float64(s.Total)
	s.Score = math.Round(timing*successRate*100) / 100
	return s
}

// trimmedStats drops the fastest and slowest 5% before taking the median and
// nearest-rank p95/p99, matching the public benchmark's scoring.
func trimmedStats(values []float64) (median, p95, p99 float64) {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	trim := len(sorted) * 5 / 100
	if trim > 0 && len(sorted)-2*trim > 0 {
		sorted = sorted[trim : len(sorted)-trim]
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	} else {
		median = sorted[mid]
	}
	return median, percentile(sorted, 95), percentile(sorted, 99)
}

func percentile(sorted []float64, p float64) float64 {
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(0, min(idx, len(sorted)-1))]
}

// metricScore scores a latency against the fixed 10s ceiling.
func metricScore(ms float64) float64 {
	return math.Max(0, 100*(1-ms/10000))
}

func msDuration(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}

func (s burstSummary) report(target string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*Burst TTI benchmark* · %s\n", target)
	fmt.Fprintf(&b, "Score *%.2f* · %d/%d succeeded\n", s.Score, s.Succeeded, s.Total)
	if s.Succeeded > 0 {
		fmt.Fprintf(&b, "TTI median %d ms · p95 %d ms · p99 %d ms\n", s.Median.Milliseconds(), s.P95.Milliseconds(), s.P99.Milliseconds())
		fmt.Fprintf(&b, "Wall clock %d ms · first ready %d ms", s.WallClock.Milliseconds(), s.FirstReady.Milliseconds())
	}
	if s.FirstError != "" {
		fmt.Fprintf(&b, "\nFirst error: %s", truncate(s.FirstError, 300))
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
