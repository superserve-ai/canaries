package loadrunner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/lock"
)

func guardTestConfig() Config {
	return Config{
		Target: stagingTarget, RunTimeout: 20 * time.Minute, DeleteTimeout: 2 * time.Minute,
		TimeZone: "America/Los_Angeles", DailyWindowStart: "07:45", DailyWindowEnd: "08:12",
		LockBackend: "gcs", LockTTL: 45 * time.Minute, GlobalLock: true, BenchmarkGuard: true,
	}
}

func guardTestQuietNow() time.Time {
	return time.Date(2026, time.July, 9, 3, 0, 0, 0, time.UTC)
}

func guardTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestScheduledWindowBudgetAndDST(t *testing.T) {
	for _, tt := range []struct {
		name, now, end string
		blocked        bool
		west           bool
	}{
		{name: "summer opening", now: "2026-07-10T14:45:00Z", end: "2026-07-10T15:12:00Z"},
		{name: "summer last complete budget", now: "2026-07-10T14:49:00Z", end: "2026-07-10T15:12:00Z"},
		{name: "summer one nanosecond late", now: "2026-07-10T14:49:00.000000001Z", blocked: true},
		{name: "before opening", now: "2026-07-10T14:44:59Z", blocked: true},
		{name: "at end", now: "2026-07-10T15:12:00Z", blocked: true},
		{name: "winter opening", now: "2026-01-09T15:45:00Z", end: "2026-01-09T16:12:00Z"},
		{name: "spring DST day", now: "2026-03-08T14:45:00Z", end: "2026-03-08T15:12:00Z"},
		{name: "autumn DST day", now: "2026-11-01T15:45:00Z", end: "2026-11-01T16:12:00Z"},
		{name: "west summer opening", now: "2026-07-10T15:15:00Z", end: "2026-07-10T15:42:00Z", west: true},
		{name: "west winter opening", now: "2026-01-09T16:15:00Z", end: "2026-01-09T16:42:00Z", west: true},
		{name: "west last complete budget", now: "2026-07-10T15:19:00Z", end: "2026-07-10T15:42:00Z", west: true},
		{name: "west late dispatch", now: "2026-07-10T15:19:01Z", blocked: true, west: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := guardTestConfig()
			if tt.west {
				cfg.DailyWindowStart, cfg.DailyWindowEnd = "08:15", "08:42"
			}
			end, err := scheduledWindow(cfg, guardTestTime(t, tt.now))
			if tt.blocked {
				if err == nil {
					t.Fatal("late or early scheduled start was admitted")
				}
				return
			}
			if err != nil || !end.Equal(guardTestTime(t, tt.end)) {
				t.Fatalf("end=%s err=%v, want %s", end, err, tt.end)
			}
		})
	}
}

func TestScheduledWindowRejectsInvalidOrUnsafeConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Config)
	}{
		{"invalid zone", func(c *Config) { c.TimeZone = "not/a-zone" }},
		{"missing zone", func(c *Config) { c.TimeZone = "" }},
		{"short clock", func(c *Config) { c.DailyWindowStart = "7:45" }},
		{"invalid clock", func(c *Config) { c.DailyWindowEnd = "25:45" }},
		{"cross midnight", func(c *Config) { c.DailyWindowStart, c.DailyWindowEnd = "23:45", "00:15" }},
		{"zero window", func(c *Config) { c.DailyWindowEnd = c.DailyWindowStart }},
		{"insufficient cleanup and report", func(c *Config) { c.DailyWindowEnd = "08:07" }},
		{"zero run", func(c *Config) { c.RunTimeout = 0 }},
		{"zero cleanup", func(c *Config) { c.DeleteTimeout = 0 }},
		{"known local heavy window", func(c *Config) { c.DailyWindowStart, c.DailyWindowEnd = "08:30", "09:00" }},
		{"known UTC heavy window", func(c *Config) { c.TimeZone, c.DailyWindowStart, c.DailyWindowEnd = "UTC", "10:00", "11:00" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := guardTestConfig()
			tt.change(&cfg)
			if _, err := scheduledWindow(cfg, guardTestTime(t, "2026-07-10T14:45:00Z")); err == nil {
				t.Fatal("unsafe scheduled configuration was admitted")
			}
		})
	}
}

func TestKnownHeavyWindowBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name, start, end string
		blocked          bool
	}{
		{"before UTC Lindy", "2026-07-09T10:00:00Z", "2026-07-09T10:30:00Z", false},
		{"enter UTC Lindy", "2026-07-09T10:29:59Z", "2026-07-09T10:30:01Z", true},
		{"leave UTC Lindy", "2026-07-09T12:29:59Z", "2026-07-09T12:30:01Z", true},
		{"after UTC Lindy", "2026-07-09T12:30:00Z", "2026-07-09T12:45:00Z", false},
		{"before midnight buffer", "2026-07-09T23:15:00Z", "2026-07-09T23:45:00Z", false},
		{"enter midnight buffer", "2026-07-09T23:44:59Z", "2026-07-09T23:45:01Z", true},
		{"midnight next day", "2026-07-10T00:00:00Z", "2026-07-10T00:30:00Z", true},
		{"leave midnight buffer", "2026-07-10T02:14:59Z", "2026-07-10T02:15:01Z", true},
		{"after midnight buffer", "2026-07-10T02:15:00Z", "2026-07-10T02:45:00Z", false},
		{"before UTC 17 buffer", "2026-01-08T16:15:00Z", "2026-01-08T16:45:00Z", false},
		{"enter UTC 17 buffer", "2026-01-08T16:44:59Z", "2026-01-08T16:45:01Z", true},
		{"leave UTC 17 buffer", "2026-01-08T18:14:59Z", "2026-01-08T18:15:01Z", true},
		{"after UTC 17 buffer", "2026-01-08T18:15:00Z", "2026-01-08T18:45:00Z", false},
		{"before Pacific Lindy summer", "2026-07-09T15:15:00Z", "2026-07-09T15:45:00Z", false},
		{"enter Pacific Lindy summer", "2026-07-09T15:44:59Z", "2026-07-09T15:45:01Z", true},
		{"Pacific Lindy winter", "2026-01-08T17:00:00Z", "2026-01-08T17:30:00Z", true},
		{"before Friday public buffer", "2026-07-10T12:30:00Z", "2026-07-10T12:45:00Z", false},
		{"enter Friday public buffer", "2026-07-10T12:44:59Z", "2026-07-10T12:45:01Z", true},
		{"leave Friday public buffer", "2026-07-10T14:44:59Z", "2026-07-10T14:45:01Z", true},
		{"after Friday public buffer", "2026-07-10T14:45:00Z", "2026-07-10T15:00:00Z", false},
		{"Thursday public time allowed", "2026-07-09T13:00:00Z", "2026-07-09T13:30:00Z", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := overlapsKnownHeavyWindow(guardTestTime(t, tt.start), guardTestTime(t, tt.end)); got != tt.blocked {
				t.Fatalf("overlap = %t, want %t", got, tt.blocked)
			}
		})
	}
}

type guardTestLocker struct {
	keys          []string
	ttls          []time.Duration
	held          map[string]bool
	leases        []*guardTestLease
	failKey       string
	failWithLease bool
	afterAcquire  func()
	now           func() time.Time
	expires       map[string]time.Time
}

type guardTestLease struct {
	locker   *guardTestLocker
	key      string
	releases int
	badCtx   bool
	fail     bool
}

func (l *guardTestLocker) Acquire(ctx context.Context, key string, ttl time.Duration) (lock.Outcome, lock.Lease, error) {
	l.keys, l.ttls = append(l.keys, key), append(l.ttls, ttl)
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if l.held == nil {
		l.held = map[string]bool{}
	}
	if expiry, exists := l.expires[key]; exists && l.now != nil && !expiry.After(l.now()) {
		delete(l.held, key)
	}
	if l.held[key] {
		return lock.OutcomeAlreadyRunning, nil, nil
	}
	if key == l.failKey && !l.failWithLease {
		return "", nil, errors.New("private backend credentials")
	}
	l.held[key] = true
	if l.now != nil {
		if l.expires == nil {
			l.expires = make(map[string]time.Time)
		}
		l.expires[key] = l.now().Add(ttl)
	}
	lease := &guardTestLease{locker: l, key: key}
	l.leases = append(l.leases, lease)
	if l.afterAcquire != nil {
		l.afterAcquire()
	}
	if key == l.failKey {
		return "", lease, errors.New("private backend credentials")
	}
	return lock.OutcomeAcquired, lease, nil
}

func (l *guardTestLease) Release(ctx context.Context) error {
	l.releases++
	deadline, bounded := ctx.Deadline()
	l.badCtx = ctx.Err() != nil || !bounded || time.Until(deadline) > guardReleaseTimeout
	delete(l.locker.held, l.key)
	delete(l.locker.expires, l.key)
	if l.fail {
		return errors.New("private release credentials")
	}
	return nil
}

func TestAcquireGuardUsesCurrentBenchmarkKeysAndReleasesOnce(t *testing.T) {
	cfg := guardTestConfig()
	cfg.LockTTL = cfg.RunTimeout + cfg.DeleteTimeout + guardReportGrace
	locker := &guardTestLocker{}
	guard, err := AcquireGuard(context.Background(), cfg, locker, guardTestQuietNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"loadtest-global", stagingTarget + "-burst", stagingTarget + "-dax", stagingTarget + "-loadtest", stagingTarget + "-soak"}
	if !reflect.DeepEqual(locker.keys, want) {
		t.Fatalf("keys = %v, want existing benchmark keys %v", locker.keys, want)
	}
	for _, ttl := range locker.ttls {
		if ttl != cfg.LockTTL {
			t.Fatalf("lease TTL = %s, want %s", ttl, cfg.LockTTL)
		}
	}
	if !guard.Deadline.IsZero() {
		t.Fatalf("manual guard deadline = %s", guard.Deadline)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := guard.Release(canceled); err != nil {
		t.Fatal(err)
	}
	if err := guard.Release(canceled); err != nil {
		t.Fatal(err)
	}
	for _, lease := range locker.leases {
		if lease.releases != 1 || lease.badCtx {
			t.Fatalf("lease %s releases=%d bad context=%t", lease.key, lease.releases, lease.badCtx)
		}
	}
	if len(locker.held) != 0 {
		t.Fatalf("leaked leases: %v", locker.held)
	}
}

type guardBenchmarkKeyRecorder struct{ key string }

func (r *guardBenchmarkKeyRecorder) Acquire(_ context.Context, key string, _ time.Duration) (lock.Outcome, lock.Lease, error) {
	r.key = key
	return "", nil, errors.New("stop benchmark before any workload starts")
}

func TestGuardLeasesMatchExistingBenchmarkRunner(t *testing.T) {
	cfg := guardTestConfig()
	locker := &guardTestLocker{}
	guard, err := AcquireGuard(context.Background(), cfg, locker, guardTestQuietNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release(context.Background())
	for _, mode := range []config.Mode{config.ModeBurst, config.ModeDax, config.ModeSoak} {
		recorder := &guardBenchmarkKeyRecorder{}
		runner := lifecycle.Runner{
			Config: config.Config{Target: cfg.Target, Mode: mode, RunTimeout: time.Second, LockTTL: cfg.LockTTL},
			Locker: recorder,
		}
		if err := runner.Run(context.Background()); err == nil {
			t.Fatal("benchmark key inspection unexpectedly ran a workload")
		}
		if !locker.held[recorder.key] {
			t.Fatalf("existing %s runner uses unprotected key %q; guard keys=%v", mode, recorder.key, locker.keys)
		}
	}
}

func TestAcquireGuardRollsBackEveryPartialLease(t *testing.T) {
	for _, tt := range []struct {
		name, held, fail string
		failWithLease    bool
	}{
		{name: "benchmark already held", held: stagingTarget + "-dax"},
		{name: "acquisition failure", fail: stagingTarget + "-dax"},
		{name: "acquisition returns lease and error", fail: stagingTarget + "-dax", failWithLease: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := guardTestConfig()
			locker := &guardTestLocker{held: map[string]bool{}, failKey: tt.fail, failWithLease: tt.failWithLease}
			if tt.held != "" {
				locker.held[tt.held] = true
			}
			guard, err := AcquireGuard(context.Background(), cfg, locker, guardTestQuietNow, nil)
			if guard != nil || !errors.Is(err, ErrGuardBlocked) || strings.Contains(err.Error(), "private") {
				t.Fatalf("guard=%v err=%v", guard, err)
			}
			for _, lease := range locker.leases {
				if lease.releases != 1 || lease.badCtx {
					t.Fatalf("lease %s not safely released: %+v", lease.key, lease)
				}
			}
			if tt.held != "" {
				delete(locker.held, tt.held)
			}
			if len(locker.held) != 0 {
				t.Fatalf("leaked leases: %v", locker.held)
			}
		})
	}
}

func TestAcquireGuardRollbackSurvivesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	locker := &guardTestLocker{afterAcquire: cancel}
	if _, err := AcquireGuard(ctx, guardTestConfig(), locker, guardTestQuietNow, nil); !errors.Is(err, ErrGuardBlocked) {
		t.Fatalf("err=%v", err)
	}
	if len(locker.leases) != 1 || locker.leases[0].releases != 1 || locker.leases[0].badCtx || len(locker.held) != 0 {
		t.Fatalf("canceled preflight leaked leases: %+v", locker)
	}
}

func TestGuardReleaseContinuesAfterFailure(t *testing.T) {
	locker := &guardTestLocker{}
	guard, err := AcquireGuard(context.Background(), guardTestConfig(), locker, guardTestQuietNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	locker.leases[len(locker.leases)-1].fail = true
	if err := guard.Release(context.Background()); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("release error = %v", err)
	}
	for _, lease := range locker.leases {
		if lease.releases != 1 {
			t.Fatalf("release failure prevented release of %s", lease.key)
		}
	}
}

func TestAcquireGuardManualLockModes(t *testing.T) {
	for _, backend := range []string{"none", "file"} {
		t.Run(backend, func(t *testing.T) {
			cfg := guardTestConfig()
			cfg.LockBackend, cfg.BenchmarkGuard, cfg.GlobalLock = backend, false, false
			cfg.TimeZone = "manual runs ignore invalid scheduled zone"
			locker := &guardTestLocker{}
			guard, err := AcquireGuard(context.Background(), cfg, locker, guardTestQuietNow, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Release(context.Background())
			wantCount := 0
			if backend == "file" {
				wantCount = 1
			}
			if len(locker.keys) != wantCount || !guard.Deadline.IsZero() {
				t.Fatalf("keys=%v deadline=%s", locker.keys, guard.Deadline)
			}
		})
	}
	cfg := guardTestConfig()
	cfg.LockBackend, cfg.BenchmarkGuard, cfg.GlobalLock = "none", false, false
	guard, err := AcquireGuard(context.Background(), cfg, lock.NoopLock{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"none", "file"} {
		cfg := guardTestConfig()
		cfg.LockBackend = backend
		if _, err := AcquireGuard(context.Background(), cfg, &guardTestLocker{}, guardTestQuietNow, nil); !errors.Is(err, ErrGuardBlocked) {
			t.Fatalf("guards on backend %s admitted: %v", backend, err)
		}
	}
}

func TestGuardedManualRunAvoidsCurrentAndUpcomingHeavyWindows(t *testing.T) {
	for _, tt := range []struct{ name, now string }{
		{"UTC Lindy running", "2026-07-09T11:00:00Z"},
		{"Pacific Lindy running", "2026-07-09T16:00:00Z"},
		{"SDK midnight running", "2026-07-09T00:30:00Z"},
		{"SDK 17 UTC running", "2026-07-09T18:00:00Z"},
		{"Friday public running", "2026-07-10T13:00:00Z"},
		{"future UTC Lindy", "2026-07-09T10:15:00Z"},
		{"future midnight buffer", "2026-07-09T23:30:00Z"},
		{"future Friday public buffer", "2026-07-10T12:35:00Z"},
		{"future Pacific and UTC 17 buffer", "2026-01-08T16:30:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := guardTestConfig()
			cfg.ExternalBenchmarkGuard = true
			requests := 0
			client := &http.Client{Transport: guardTestTransport(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(guardEmptyRuns))}, nil
			})}
			locker := &guardTestLocker{}
			current := guardTestTime(t, tt.now)
			guard, err := AcquireGuard(context.Background(), cfg, locker, func() time.Time { return current }, client)
			if guard != nil || !errors.Is(err, ErrGuardBlocked) || len(locker.keys) != 0 || requests != 0 {
				t.Fatalf("heavy manual span guard=%v err=%v leases=%v requests=%d", guard, err, locker.keys, requests)
			}
		})
	}
}

func TestGuardedManualRunAcceptsQuietTimeOutsideDailySlot(t *testing.T) {
	cfg := guardTestConfig()
	cfg.Scenario = "create"
	cfg.TimeZone = "manual runs do not require a daily slot"
	locker := &guardTestLocker{}
	guard, err := AcquireGuard(context.Background(), cfg, locker, guardTestQuietNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release(context.Background())
	if !guard.Deadline.IsZero() || len(locker.keys) != 5 {
		t.Fatalf("quiet manual create run deadline=%s leases=%v", guard.Deadline, locker.keys)
	}
}

func TestUnguardedLocalManualRunRetainsHeavyWindowOptOut(t *testing.T) {
	cfg := guardTestConfig()
	cfg.LockBackend, cfg.BenchmarkGuard, cfg.GlobalLock = "none", false, false
	current := guardTestTime(t, "2026-07-10T13:00:00Z")
	guard, err := AcquireGuard(context.Background(), cfg, lock.NoopLock{}, func() time.Time { return current }, nil)
	if err != nil {
		t.Fatalf("unguarded local manual run was blocked: %v", err)
	}
	if !guard.Deadline.IsZero() {
		t.Fatalf("local manual deadline = %s", guard.Deadline)
	}
	if err := guard.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGuardedManualRunRechecksFutureHeavyBoundaryAfterPreflight(t *testing.T) {
	for _, phase := range []string{"external", "leases"} {
		t.Run(phase, func(t *testing.T) {
			cfg := guardTestConfig()
			current := guardTestTime(t, "2026-07-09T10:06:00Z")
			late := guardTestTime(t, "2026-07-09T10:08:00Z")
			locker := &guardTestLocker{}
			var client *http.Client
			if phase == "external" {
				cfg.ExternalBenchmarkGuard = true
				client = &http.Client{Transport: guardTestTransport(func(*http.Request) (*http.Response, error) {
					current = late
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(guardEmptyRuns))}, nil
				})}
			} else {
				locker.afterAcquire = func() { current = late }
			}
			guard, err := AcquireGuard(context.Background(), cfg, locker, func() time.Time { return current }, client)
			if guard != nil || !errors.Is(err, ErrGuardBlocked) || len(locker.held) != 0 {
				t.Fatalf("late manual preflight guard=%v err=%v leases=%v", guard, err, locker.held)
			}
			if phase == "external" && len(locker.keys) != 0 {
				t.Fatalf("external time recheck acquired leases: %v", locker.keys)
			}
			if phase == "leases" && len(locker.leases) != 5 {
				t.Fatalf("rollback fixture acquired %d leases, want 5", len(locker.leases))
			}
			for _, lease := range locker.leases {
				if lease.releases != 1 || lease.badCtx {
					t.Fatalf("late manual preflight leaked lease %s", lease.key)
				}
			}
		})
	}
}

func TestAcquireGuardScheduledAbsoluteDeadlineAndLateLockRollback(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint("late=", late), func(t *testing.T) {
			cfg := guardTestConfig()
			cfg.Scheduled = true
			current := guardTestTime(t, "2026-07-10T14:45:00Z")
			locker := &guardTestLocker{}
			locker.afterAcquire = func() {
				if late {
					current = guardTestTime(t, "2026-07-10T14:49:01Z")
				}
			}
			guard, err := AcquireGuard(context.Background(), cfg, locker, func() time.Time { return current }, nil)
			if late {
				if !errors.Is(err, ErrGuardBlocked) || guard != nil || len(locker.held) != 0 {
					t.Fatalf("late preflight guard=%v err=%v held=%v", guard, err, locker.held)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Release(context.Background())
			want := guardTestTime(t, "2026-07-10T15:09:00Z")
			if !guard.Deadline.Equal(want) {
				t.Fatalf("work deadline=%s, want cleanup and reporting reservation at %s", guard.Deadline, want)
			}
		})
	}
}

func TestScheduledAdmissionSurvivesCompletionAndExpiresForNextDay(t *testing.T) {
	cfg := guardTestConfig()
	cfg.Scheduled = true
	current := guardTestTime(t, "2026-07-10T14:45:00Z")
	now := func() time.Time { return current }
	locker := &guardTestLocker{now: now}
	markerKey := cfg.Target + "-loadtest-daily"
	guard, err := AcquireGuard(context.Background(), cfg, locker, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if locker.keys[len(locker.keys)-1] != markerKey {
		t.Fatalf("admission marker was not acquired last: %v", locker.keys)
	}
	if err := guard.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(locker.held, map[string]bool{markerKey: true}) {
		t.Fatalf("completion retained shared leases or lost admission: %v", locker.held)
	}
	markerLease := locker.leases[len(locker.leases)-1]
	if markerLease.releases != 0 {
		t.Fatal("completed run released its daily admission")
	}
	current = current.Add(time.Minute)
	if duplicate, err := AcquireGuard(context.Background(), cfg, locker, now, nil); duplicate != nil || !errors.Is(err, ErrGuardBlocked) {
		t.Fatalf("sequential duplicate admitted: guard=%v err=%v", duplicate, err)
	}
	if !reflect.DeepEqual(locker.held, map[string]bool{markerKey: true}) {
		t.Fatalf("duplicate did not roll back shared leases: %v", locker.held)
	}
	current = guardTestTime(t, "2026-07-11T14:45:00Z")
	nextDay, err := AcquireGuard(context.Background(), cfg, locker, now, nil)
	if err != nil {
		t.Fatalf("expired admission prevented next day's run: %v", err)
	}
	if err := nextDay.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(locker.held, map[string]bool{markerKey: true}) {
		t.Fatalf("next-day completion held unexpected keys: %v", locker.held)
	}
}

func TestScheduledAdmissionMarkerCoversEntireAllowedWindow(t *testing.T) {
	cfg := guardTestConfig()
	cfg.Scheduled = true
	cfg.LockTTL = cfg.RunTimeout + cfg.DeleteTimeout + guardReportGrace
	current := guardTestTime(t, "2026-07-10T14:45:00Z")
	locker := &guardTestLocker{now: func() time.Time { return current }}
	guard, err := AcquireGuard(context.Background(), cfg, locker, locker.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release(context.Background())
	markerExpiry := locker.expires[cfg.Target+"-loadtest-daily"]
	windowEnd := guardTestTime(t, "2026-07-10T15:12:00Z")
	if !markerExpiry.After(windowEnd) {
		t.Fatalf("marker expiry %s does not cover allowed window through %s", markerExpiry, windowEnd)
	}
}

func TestScheduledAdmissionFailureRollsBackAllWorkloadLeases(t *testing.T) {
	for _, failure := range []string{"held", "acquire", "acquire with lease", "late", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			cfg := guardTestConfig()
			cfg.Scheduled = true
			markerKey := cfg.Target + "-loadtest-daily"
			current := guardTestTime(t, "2026-07-10T14:45:00Z")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			locker := &guardTestLocker{held: map[string]bool{}, now: func() time.Time { return current }}
			switch failure {
			case "held":
				locker.held[markerKey] = true
			case "acquire", "acquire with lease":
				locker.failKey = markerKey
				locker.failWithLease = failure == "acquire with lease"
			case "late", "canceled":
				locker.afterAcquire = func() {
					if locker.keys[len(locker.keys)-1] != markerKey {
						return
					}
					if failure == "late" {
						current = guardTestTime(t, "2026-07-10T14:49:01Z")
					} else {
						cancel()
					}
				}
			}
			guard, err := AcquireGuard(ctx, cfg, locker, locker.now, nil)
			if guard != nil || !errors.Is(err, ErrGuardBlocked) || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe marker preflight admitted: guard=%v err=%v", guard, err)
			}
			for _, lease := range locker.leases {
				if lease.releases != 1 || lease.badCtx {
					t.Fatalf("preflight left acquired lease %s unreleased: %+v", lease.key, lease)
				}
			}
			if failure == "held" {
				delete(locker.held, markerKey)
			}
			if len(locker.held) != 0 {
				t.Fatalf("failed admission leaked leases: %v", locker.held)
			}
		})
	}
}

type guardTestTransport func(*http.Request) (*http.Response, error)

func (f guardTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type guardTestBody struct {
	io.Reader
	closed bool
}

func (b *guardTestBody) Close() error { b.closed = true; return nil }

func guardTestResponse(body string) string { return `{"total_count":1,"workflow_runs":[` + body + `]}` }

const guardEmptyRuns = `{"total_count":0,"workflow_runs":[]}`

func TestExternalBenchmarkGuardFixtures(t *testing.T) {
	for _, tt := range []struct {
		name, first, second string
		statusCode          int
		link                string
		networkError        bool
		blocked             bool
		calls               int
	}{
		{name: "empty", first: guardEmptyRuns, second: guardEmptyRuns, calls: 2},
		{name: "queued heavy", first: guardTestResponse(`{"name":"Sandbox TTI","path":".github/workflows/sandbox-tti.yml"}`), blocked: true, calls: 1},
		{name: "active heavy", first: guardEmptyRuns, second: guardTestResponse(`{"name":"Sandbox DAX","path":".github/workflows/sandbox-dax.yml"}`), blocked: true, calls: 2},
		{name: "unknown sandbox", first: guardTestResponse(`{"name":"Sandbox New Benchmark","path":".github/workflows/custom.yml"}`), blocked: true, calls: 1},
		{name: "path identifies heavy", first: guardTestResponse(`{"name":"New load","path":".github/workflows/sandbox-new.yml"}`), blocked: true, calls: 1},
		{name: "reliability", first: guardTestResponse(`{"name":"Sandbox Reliability","path":".github/workflows/sandbox-reliability.yml"}`), second: guardEmptyRuns, calls: 2},
		{name: "provider reliability", first: guardTestResponse(`{"name":"Sandbox Provider Reliability","path":".github/workflows/sandbox-reliability.yml"}`), second: guardEmptyRuns, calls: 2},
		{name: "continuous reliability probe", first: guardTestResponse(`{"name":"Sandbox Continuous Reliability Probe","path":".github/workflows/sandbox-reliability.yml"}`), second: guardEmptyRuns, calls: 2},
		{name: "reliability soak still blocks", first: guardTestResponse(`{"name":"Sandbox Reliability Soak","path":".github/workflows/sandbox-reliability.yml"}`), blocked: true, calls: 1},
		{name: "reliability name with heavy path blocks", first: guardTestResponse(`{"name":"Sandbox Reliability","path":".github/workflows/sandbox-soak.yml"}`), blocked: true, calls: 1},
		{name: "unrelated workflow", first: guardTestResponse(`{"name":"Unit tests","path":".github/workflows/ci.yml"}`), second: guardEmptyRuns, calls: 2},
		{name: "malformed", first: `{"total_count":`, blocked: true, calls: 1},
		{name: "missing count", first: `{"workflow_runs":[]}`, blocked: true, calls: 1},
		{name: "missing list", first: `{"total_count":0}`, blocked: true, calls: 1},
		{name: "null list", first: `{"total_count":0,"workflow_runs":null}`, blocked: true, calls: 1},
		{name: "missing name", first: guardTestResponse(`{"path":".github/workflows/ci.yml"}`), blocked: true, calls: 1},
		{name: "missing path", first: guardTestResponse(`{"name":"Unit tests"}`), blocked: true, calls: 1},
		{name: "truncated pagination", first: `{"total_count":101,"workflow_runs":[]}`, blocked: true, calls: 1},
		{name: "next page link", first: guardEmptyRuns, link: `<https://api.github.com/?page=2>; rel="next"`, blocked: true, calls: 1},
		{name: "oversized body", first: strings.Repeat(" ", guardMaxResponseBytes+1), blocked: true, calls: 1},
		{name: "HTTP unavailable", first: "private response credentials", statusCode: http.StatusForbidden, blocked: true, calls: 1},
		{name: "network unavailable", networkError: true, blocked: true, calls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			var bodies []*guardTestBody
			client := &http.Client{Transport: guardTestTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				status := "queued"
				body := tt.first
				if calls == 2 {
					status, body = "in_progress", tt.second
				}
				if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Host != "api.github.com" || req.URL.Path != "/repos/computesdk/benchmarks/actions/runs" || req.URL.Query().Get("status") != status || req.URL.Query().Get("per_page") != "100" {
					t.Fatalf("unexpected public endpoint request: %s %s", req.Method, req.URL)
				}
				if req.Header.Get("Authorization") != "" || req.Header.Get("X-API-Key") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("User-Agent") != "superserve-canaries-load-test-guard" {
					t.Fatalf("unexpected request headers: %v", req.Header)
				}
				deadline, bounded := req.Context().Deadline()
				if !bounded || time.Until(deadline) > guardRequestTimeout {
					t.Fatal("external request lacks bounded time budget")
				}
				if tt.networkError {
					return nil, errors.New("private transport credentials")
				}
				statusCode := tt.statusCode
				if statusCode == 0 {
					statusCode = http.StatusOK
				}
				tracked := &guardTestBody{Reader: strings.NewReader(body)}
				bodies = append(bodies, tracked)
				return &http.Response{StatusCode: statusCode, Header: http.Header{"Link": []string{tt.link}}, Body: tracked}, nil
			})}
			cfg := guardTestConfig()
			cfg.APIKey, cfg.ExternalBenchmarkGuard = "private canary API key", true
			locker := &guardTestLocker{}
			guard, err := AcquireGuard(context.Background(), cfg, locker, guardTestQuietNow, client)
			if tt.blocked {
				if guard != nil || !errors.Is(err, ErrGuardBlocked) || strings.Contains(err.Error(), "private") || len(locker.keys) != 0 {
					t.Fatalf("guard=%v err=%v keys=%v", guard, err, locker.keys)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := guard.Release(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if calls != tt.calls {
				t.Fatalf("calls=%d, want %d", calls, tt.calls)
			}
			for _, body := range bodies {
				if !body.closed {
					t.Fatal("external response body leaked")
				}
			}
		})
	}
}

func TestExternalGuardCatchesQueuedToRunningTransition(t *testing.T) {
	state := "queued"
	client := &http.Client{Transport: guardTestTransport(func(req *http.Request) (*http.Response, error) {
		body := guardEmptyRuns
		if req.URL.Query().Get("status") == state {
			body = guardTestResponse(`{"name":"Sandbox TTI","path":".github/workflows/sandbox-tti.yml"}`)
		}
		// Simulate the queued workflow starting immediately after the first
		// snapshot. Checking running then queued would miss this workflow.
		state = "in_progress"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	cfg := guardTestConfig()
	cfg.ExternalBenchmarkGuard = true
	locker := &guardTestLocker{}
	if _, err := AcquireGuard(context.Background(), cfg, locker, guardTestQuietNow, client); !errors.Is(err, ErrGuardBlocked) {
		t.Fatalf("queued-to-running heavy benchmark was admitted: %v", err)
	}
	if len(locker.keys) != 0 {
		t.Fatalf("external block unexpectedly acquired leases: %v", locker.keys)
	}
}

func TestAcquireGuardRechecksTimeAfterExternalRequests(t *testing.T) {
	cfg := guardTestConfig()
	cfg.Scheduled, cfg.ExternalBenchmarkGuard = true, true
	current := guardTestTime(t, "2026-07-10T14:45:00Z")
	calls := 0
	client := &http.Client{Transport: guardTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		current = guardTestTime(t, "2026-07-10T14:49:01Z")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(guardEmptyRuns))}, nil
	})}
	locker := &guardTestLocker{}
	if _, err := AcquireGuard(context.Background(), cfg, locker, func() time.Time { return current }, client); !errors.Is(err, ErrGuardBlocked) {
		t.Fatalf("late external preflight admitted: %v", err)
	}
	if calls != 2 || len(locker.keys) != 0 {
		t.Fatalf("requests=%d leases=%v", calls, locker.keys)
	}
}
