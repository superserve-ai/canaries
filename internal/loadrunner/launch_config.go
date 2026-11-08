package loadrunner

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const ScenarioLaunch = "launch"

const (
	// MaxLaunchOperations bounds the aggregate launch size across every
	// generator task. Each task still runs at most MaxOperations sandboxes.
	MaxLaunchOperations      = 1_000_000
	defaultLaunchCeiling     = 100
	defaultLaunchConcurrency = 256
	defaultLaunchTemplate    = "superserve/node-22"
	// The sandbox API limits each team to 50 requests/s per API instance,
	// including status polls. The default budget leaves room for other
	// callers on the same team.
	defaultLaunchAPIRate       = 40
	maxLaunchRate              = 100_000
	maxTeardownTimeout         = time.Hour
	maxTeardownConcurrency     = 1024
	defaultTeardownConcurrency = 32
)

// LaunchConfig configures the mass-launch scenario. Rates are aggregate
// across all generator tasks; each task paces its own share.
type LaunchConfig struct {
	Strategy            string
	Rate                float64
	RampStartRate       float64
	RampStepRate        float64
	RampStepDuration    time.Duration
	Hold                time.Duration
	Ceiling             int
	CeilingExplicit     bool
	APIRate             float64
	TeardownTimeout     time.Duration
	TeardownConcurrency int
	TaskIndex           int
	TaskCount           int
}

func loadLaunchConfig() (LaunchConfig, error) {
	cfg := LaunchConfig{
		Strategy:            envDefault("LOAD_TEST_LAUNCH_STRATEGY", "burst"),
		Rate:                envFloat("LOAD_TEST_LAUNCH_RATE", 0),
		RampStartRate:       envFloat("LOAD_TEST_RAMP_START_RATE", 1),
		RampStepRate:        envFloat("LOAD_TEST_RAMP_STEP_RATE", 1),
		RampStepDuration:    envDuration("LOAD_TEST_RAMP_STEP_DURATION", 10*time.Second),
		Hold:                envDuration("LOAD_TEST_LAUNCH_HOLD", 0),
		Ceiling:             envInt("LOAD_TEST_LAUNCH_CEILING", defaultLaunchCeiling),
		CeilingExplicit:     strings.TrimSpace(os.Getenv("LOAD_TEST_LAUNCH_CEILING")) != "",
		APIRate:             envFloat("LOAD_TEST_API_RPS", defaultLaunchAPIRate),
		TeardownTimeout:     envDuration("LOAD_TEST_TEARDOWN_TIMEOUT", 15*time.Minute),
		TeardownConcurrency: envInt("LOAD_TEST_TEARDOWN_CONCURRENCY", defaultTeardownConcurrency),
	}
	var err error
	if cfg.TaskIndex, err = taskEnvInt("LOAD_TEST_TASK_INDEX", "CLOUD_RUN_TASK_INDEX", 0); err != nil {
		return LaunchConfig{}, err
	}
	if cfg.TaskCount, err = taskEnvInt("LOAD_TEST_TASK_COUNT", "CLOUD_RUN_TASK_COUNT", 1); err != nil {
		return LaunchConfig{}, err
	}
	return cfg, nil
}

func taskEnvInt(primary, cloudRun string, fallback int) (int, error) {
	name := primary
	value := strings.TrimSpace(os.Getenv(primary))
	if value == "" {
		name = cloudRun
		value = strings.TrimSpace(os.Getenv(cloudRun))
	}
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func validateLaunchConfig(cfg Config) error {
	l := cfg.Launch
	if l.TaskCount < 1 || l.TaskIndex < 0 || l.TaskIndex >= l.TaskCount {
		return errors.New("LOAD_TEST_TASK_INDEX must be in [0, LOAD_TEST_TASK_COUNT)")
	}
	if l.Ceiling < 1 || l.Ceiling > MaxLaunchOperations {
		return fmt.Errorf("LOAD_TEST_LAUNCH_CEILING must be between 1 and %d", MaxLaunchOperations)
	}
	if cfg.Operations <= 0 || cfg.Operations > l.Ceiling {
		return fmt.Errorf("LOAD_TEST_OPERATIONS must be between 1 and LOAD_TEST_LAUNCH_CEILING (%d); raise the ceiling explicitly for larger launches", l.Ceiling)
	}
	share := l.TaskOperations(cfg.Operations)
	if share > MaxOperations {
		return fmt.Errorf("each launch task may create at most %d sandboxes; raise LOAD_TEST_TASK_COUNT", MaxOperations)
	}
	if share < 1 {
		return errors.New("LOAD_TEST_TASK_COUNT must not exceed LOAD_TEST_OPERATIONS")
	}
	if cfg.Concurrency <= 0 || cfg.Concurrency > share {
		return errors.New("LOAD_TEST_CONCURRENCY must be positive and no greater than this task's launch share")
	}
	switch l.Strategy {
	case "burst":
		if l.Rate < 0 || l.Rate > maxLaunchRate {
			return fmt.Errorf("LOAD_TEST_LAUNCH_RATE must be between 0 and %d", maxLaunchRate)
		}
	case "ramp":
		if l.Rate <= 0 || l.Rate > maxLaunchRate || l.RampStartRate <= 0 || l.RampStartRate > l.Rate || l.RampStepRate <= 0 || l.RampStepDuration < time.Second {
			return errors.New("ramp launches require 0 < LOAD_TEST_RAMP_START_RATE <= LOAD_TEST_LAUNCH_RATE, a positive LOAD_TEST_RAMP_STEP_RATE, and LOAD_TEST_RAMP_STEP_DURATION >= 1s")
		}
	default:
		return errors.New("LOAD_TEST_LAUNCH_STRATEGY must be burst or ramp")
	}
	if l.APIRate <= 0 || l.APIRate > maxLaunchRate {
		return fmt.Errorf("LOAD_TEST_API_RPS must be between 0 and %d", maxLaunchRate)
	}
	if l.Hold < 0 || l.Hold >= cfg.RunTimeout {
		return errors.New("LOAD_TEST_LAUNCH_HOLD must be non-negative and shorter than LOAD_TEST_RUN_TIMEOUT")
	}
	if l.TeardownTimeout <= 0 || l.TeardownTimeout > maxTeardownTimeout || l.TeardownConcurrency <= 0 || l.TeardownConcurrency > maxTeardownConcurrency {
		return fmt.Errorf("LOAD_TEST_TEARDOWN_TIMEOUT must be in (0, %s] and LOAD_TEST_TEARDOWN_CONCURRENCY in [1, %d]", maxTeardownTimeout, maxTeardownConcurrency)
	}
	if l.TaskCount > 1 && cfg.LockBackend != "none" {
		// Fan-out tasks cannot share one lease yet (SS-346); each would
		// contend for the same target lock.
		return errors.New("launch fan-out (LOAD_TEST_TASK_COUNT > 1) requires CANARY_LOCK_BACKEND=none until shared leases exist")
	}
	if err := validatePressureConfig(cfg.Pressure); err != nil {
		return err
	}
	if cfg.Environment == "production" {
		if l.TaskCount != 1 {
			return errors.New("production launches must run as a single task")
		}
		if cfg.LockBackend != "gcs" || !cfg.GlobalLock || !cfg.BenchmarkGuard {
			return errors.New("production launches require GCS coordination with the global lock and benchmark guard")
		}
		if !l.CeilingExplicit {
			return errors.New("production launches require an explicit LOAD_TEST_LAUNCH_CEILING")
		}
		if cfg.Pressure.Mode != PressureModeEnforce || !cfg.Pressure.HasHostSource() {
			return errors.New("production launches require LOAD_TEST_GUARD_MODE=enforce with a host pressure source")
		}
	}
	return nil
}

// TaskOperations returns how many of total operations this task owns. Task i
// owns every operation index n with n % TaskCount == i.
func (l LaunchConfig) TaskOperations(total int) int {
	count := max(l.TaskCount, 1)
	if l.TaskIndex >= count || total <= l.TaskIndex {
		return 0
	}
	return (total - l.TaskIndex + count - 1) / count
}

// TaskRate converts an aggregate rate into this task's share.
func (l LaunchConfig) TaskRate(rate float64) float64 {
	return rate / float64(max(l.TaskCount, 1))
}

// CleanupBudget is the time reserved after RunTimeout for removing every
// sandbox the run created.
func (c Config) CleanupBudget() time.Duration {
	if c.Scenario == ScenarioLaunch {
		return c.DeleteTimeout + c.Launch.TeardownTimeout
	}
	return c.DeleteTimeout
}
