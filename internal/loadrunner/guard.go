package loadrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/superserve-ai/canaries/internal/lock"
)

var ErrGuardBlocked = errors.New("load test preflight blocked")

const (
	guardReportGrace      = time.Minute
	guardPreflightTimeout = 30 * time.Second
	guardRequestTimeout   = 10 * time.Second
	guardReleaseTimeout   = 10 * time.Second
	guardMaxResponseBytes = 1 << 20
	benchmarkRunsURL      = "https://api.github.com/repos/computesdk/benchmarks/actions/runs"
)

// Guard owns the leases held for a load test. Deadline reserves the scheduled
// window's final cleanup and reporting time; manual runs have no such deadline.
type Guard struct {
	Deadline time.Time
	mu       sync.Mutex
	leases   []lock.Lease
}

// Release attempts every workload lease even when the run context has been
// canceled. Successful scheduled admission remains recorded until its TTL.
// Lease release is bounded independently from the workload's time budget.
func (g *Guard) Release(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	leases := g.leases
	g.leases = nil
	g.mu.Unlock()
	if len(leases) == 0 {
		return nil
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), guardReleaseTimeout)
	defer cancel()
	failed := false
	for i := len(leases) - 1; i >= 0; i-- {
		if err := leases[i].Release(releaseCtx); err != nil {
			failed = true
		}
	}
	if failed {
		return errors.New("load test guard lease release failed")
	}
	return nil
}

// AcquireGuard admits work only after the time window, known external heavy
// workflows, and all requested leases have been checked. It never starts work.
func AcquireGuard(ctx context.Context, cfg Config, locker lock.Lock, now func() time.Time, httpClient *http.Client) (*Guard, error) {
	if now == nil {
		now = time.Now
	}
	preflightCtx, cancel := context.WithTimeout(ctx, guardPreflightTimeout)
	defer cancel()
	guard := &Guard{}
	blocked := func(reason string) (*Guard, error) {
		if err := guard.Release(ctx); err != nil {
			return nil, fmt.Errorf("%w: %s; lease release failed", ErrGuardBlocked, reason)
		}
		return nil, fmt.Errorf("%w: %s", ErrGuardBlocked, reason)
	}
	var windowEnd time.Time
	checkTime := func() error {
		if preflightCtx.Err() != nil {
			return errors.New("preflight time budget exhausted")
		}
		if !cfg.Scheduled {
			if cfg.BenchmarkGuard || cfg.ExternalBenchmarkGuard {
				start := now()
				if cfg.RunTimeout <= 0 || cfg.DeleteTimeout <= 0 {
					return errors.New("invalid guarded load test budget")
				}
				if overlapsKnownHeavyWindow(start, start.Add(cfg.RunTimeout+cfg.DeleteTimeout+guardReportGrace)) {
					return errors.New("load test budget overlaps a known heavy benchmark window")
				}
			}
			return nil
		}
		end, err := scheduledWindow(cfg, now())
		if err != nil {
			return err
		}
		if !windowEnd.IsZero() && !windowEnd.Equal(end) {
			return errors.New("scheduled day changed during preflight")
		}
		windowEnd = end
		guard.Deadline = end.Add(-cfg.DeleteTimeout - guardReportGrace)
		return nil
	}
	if err := checkTime(); err != nil {
		return blocked(err.Error())
	}
	if cfg.ExternalBenchmarkGuard {
		if err := checkExternalBenchmarks(preflightCtx, httpClient); err != nil {
			return blocked(err.Error())
		}
		if err := checkTime(); err != nil {
			return blocked(err.Error())
		}
	}
	backend := strings.TrimSpace(cfg.LockBackend)
	if cfg.Scheduled && backend != "gcs" {
		return blocked("scheduled admission requires the shared lock backend")
	}
	if backend != "" && backend != "none" {
		if locker == nil || cfg.Target == "" || cfg.LockTTL < cfg.RunTimeout+cfg.DeleteTimeout+guardReportGrace {
			return blocked("invalid load test lease configuration")
		}
		if (cfg.GlobalLock || cfg.BenchmarkGuard) && backend != "gcs" {
			return blocked("benchmark and global leases require the shared lock backend")
		}
		keys := []string{cfg.Target + "-loadtest"}
		if cfg.GlobalLock {
			keys = append(keys, "loadtest-global")
		}
		if cfg.BenchmarkGuard {
			// These are the exact keys used by the existing benchmark runner.
			keys = append(keys, cfg.Target+"-burst", cfg.Target+"-dax", cfg.Target+"-soak")
		}
		sort.Strings(keys)
		for _, key := range keys {
			outcome, lease, err := locker.Acquire(preflightCtx, key, cfg.LockTTL)
			if lease != nil {
				guard.leases = append(guard.leases, lease)
			}
			if err != nil || outcome != lock.OutcomeAcquired || lease == nil {
				return blocked("load test or benchmark lease unavailable")
			}
		}
	} else if cfg.GlobalLock || cfg.BenchmarkGuard {
		return blocked("benchmark and global leases require the shared lock backend")
	}
	if err := checkTime(); err != nil {
		return blocked(err.Error())
	}
	if cfg.Scheduled {
		// Keep a separate admission marker after workload cleanup. Scheduler
		// redelivery must not repeat a fast completed run in the same window.
		// Reuse one key; expired markers are replaced by the next day's run.
		markerTTL := max(cfg.LockTTL, windowEnd.Sub(now())+guardReportGrace)
		outcome, lease, err := locker.Acquire(preflightCtx, cfg.Target+"-loadtest-daily", markerTTL)
		if lease != nil {
			guard.leases = append(guard.leases, lease)
		}
		if err != nil || outcome != lock.OutcomeAcquired || lease == nil {
			return blocked("daily load test admission unavailable")
		}
		if err := checkTime(); err != nil {
			return blocked(err.Error())
		}
		// All preflight checks passed. Retain only this marker through expiry;
		// Release still frees the shared benchmark and workload leases.
		guard.leases = guard.leases[:len(guard.leases)-1]
	}
	return guard, nil
}

func scheduledWindow(cfg Config, now time.Time) (time.Time, error) {
	location, err := time.LoadLocation(cfg.TimeZone)
	if err != nil || cfg.TimeZone == "" {
		return time.Time{}, errors.New("invalid scheduled time zone")
	}
	startHour, startMinute, err := parseWindowClock(cfg.DailyWindowStart)
	if err != nil {
		return time.Time{}, err
	}
	endHour, endMinute, err := parseWindowClock(cfg.DailyWindowEnd)
	if err != nil {
		return time.Time{}, err
	}
	localNow := now.In(location)
	start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), startHour, startMinute, 0, 0, location)
	end := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), endHour, endMinute, 0, 0, location)
	if !end.After(start) || start.Hour() != startHour || start.Minute() != startMinute || end.Hour() != endHour || end.Minute() != endMinute {
		return time.Time{}, errors.New("scheduled window must fit within one local day")
	}
	if cfg.RunTimeout <= 0 || cfg.DeleteTimeout <= 0 || cfg.RunTimeout+cfg.DeleteTimeout+guardReportGrace > end.Sub(start) {
		return time.Time{}, errors.New("scheduled window cannot fit the complete load test budget")
	}
	if overlapsKnownHeavyWindow(start, end) {
		return time.Time{}, errors.New("scheduled window overlaps a known heavy benchmark window")
	}
	if now.Before(start) || !now.Before(end) || now.Add(cfg.RunTimeout+cfg.DeleteTimeout+guardReportGrace).After(end) {
		return time.Time{}, errors.New("outside the scheduled load test start window")
	}
	return end, nil
}

func parseWindowClock(value string) (int, int, error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, 0, errors.New("invalid scheduled window clock")
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, 0, errors.New("invalid scheduled window clock")
	}
	return parsed.Hour(), parsed.Minute(), nil
}

func overlapsKnownHeavyWindow(start, end time.Time) bool {
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return true
	}
	// Check adjacent dates too, since the observed midnight heavy workflows and
	// their safety buffers cross a UTC day boundary.
	for _, location := range []*time.Location{time.UTC, pacific} {
		local := start.In(location)
		date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
		for offset := -1; offset <= 1; offset++ {
			day := date.AddDate(0, 0, offset)
			var intervals [][2]time.Time
			if location == time.UTC {
				intervals = append(intervals,
					[2]time.Time{day.Add(10*time.Hour + 30*time.Minute), day.Add(12*time.Hour + 30*time.Minute)},
					[2]time.Time{day.Add(-15 * time.Minute), day.Add(2*time.Hour + 15*time.Minute)},
					[2]time.Time{day.Add(16*time.Hour + 45*time.Minute), day.Add(18*time.Hour + 15*time.Minute)},
				)
				if day.Weekday() == time.Friday {
					intervals = append(intervals, [2]time.Time{day.Add(12*time.Hour + 45*time.Minute), day.Add(14*time.Hour + 45*time.Minute)})
				}
			} else {
				intervals = append(intervals, [2]time.Time{
					time.Date(day.Year(), day.Month(), day.Day(), 8, 45, 0, 0, location),
					time.Date(day.Year(), day.Month(), day.Day(), 9, 45, 0, 0, location),
				})
			}
			for _, interval := range intervals {
				if start.Before(interval[1]) && end.After(interval[0]) {
					return true
				}
			}
		}
	}
	return false
}

func checkExternalBenchmarks(ctx context.Context, client *http.Client) error {
	if client == nil {
		client = &http.Client{Timeout: guardRequestTimeout}
	}
	// Redirects are unnecessary for this fixed public endpoint. Refuse them so
	// preflight never follows an unexpected destination or credentials challenge.
	requestClient := *client
	requestClient.Jar = nil
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
	// A queued run can become active between requests. Check that state first
	// so this forward transition cannot disappear from both responses.
	for _, status := range []string{"queued", "in_progress"} {
		if err := checkExternalBenchmarkStatus(ctx, &requestClient, status); err != nil {
			return err
		}
	}
	return nil
}

func checkExternalBenchmarkStatus(ctx context.Context, client *http.Client, status string) error {
	requestCtx, cancel := context.WithTimeout(ctx, guardRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, benchmarkRunsURL+"?status="+status+"&per_page=100", nil)
	if err != nil {
		return errors.New("external benchmark state unavailable")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "superserve-canaries-load-test-guard")
	response, err := client.Do(req)
	if err != nil {
		return errors.New("external benchmark state unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || strings.Contains(response.Header.Get("Link"), `rel="next"`) {
		return errors.New("external benchmark state unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, guardMaxResponseBytes+1))
	if err != nil || len(body) > guardMaxResponseBytes {
		return errors.New("external benchmark state unavailable")
	}
	var payload struct {
		TotalCount *int            `json:"total_count"`
		Runs       json.RawMessage `json:"workflow_runs"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.TotalCount == nil || *payload.TotalCount < 0 || len(payload.Runs) == 0 || string(payload.Runs) == "null" {
		return errors.New("external benchmark state unavailable")
	}
	var runs []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if json.Unmarshal(payload.Runs, &runs) != nil || *payload.TotalCount != len(runs) {
		return errors.New("external benchmark state unavailable")
	}
	for _, run := range runs {
		name, path := strings.TrimSpace(run.Name), strings.TrimSpace(run.Path)
		if name == "" || path == "" {
			return errors.New("external benchmark state unavailable")
		}
		if isHeavySandboxWorkflow(name, path) {
			return errors.New("external heavy benchmark active or queued")
		}
	}
	return nil
}

func isHeavySandboxWorkflow(name, path string) bool {
	name, path = strings.ToLower(name), strings.ToLower(path)
	path = strings.SplitN(path, "@", 2)[0]
	heavy := strings.HasPrefix(name, "sandbox") || strings.HasPrefix(path, ".github/workflows/sandbox-") || path == ".github/workflows/sandbox.yml" || path == ".github/workflows/sandbox.yaml"
	if !heavy {
		return false
	}
	// Only the known-style continuous reliability probe is exempt. A sandbox
	// soak/load/benchmark workflow mentioning reliability must still block.
	reliabilityName := name == "sandbox reliability" || name == "sandbox provider reliability" || name == "sandbox continuous reliability probe"
	reliabilityPath := path == ".github/workflows/sandbox-reliability.yml" || path == ".github/workflows/sandbox-reliability.yaml"
	return !(reliabilityName && reliabilityPath)
}
