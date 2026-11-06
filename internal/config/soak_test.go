package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadSoakDefaultsProtectFullRunAndAlwaysCleanup(t *testing.T) {
	soakTestEnv(t)
	t.Setenv("CANARY_RETAIN_FAILED_SANDBOX", "true")
	cfg, err := Load("soak")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeSoak || cfg.SoakDuration != 6*time.Hour || cfg.SoakInterval != time.Minute {
		t.Fatalf("unexpected soak configuration: mode=%s duration=%s interval=%s", cfg.Mode, cfg.SoakDuration, cfg.SoakInterval)
	}
	if cfg.RunTimeout != 6*time.Hour+15*time.Minute || cfg.LockTTL != 6*time.Hour+30*time.Minute || cfg.ResourceTTL != cfg.LockTTL {
		t.Fatalf("unsafe default budgets: run=%s lock=%s resource=%s", cfg.RunTimeout, cfg.LockTTL, cfg.ResourceTTL)
	}
	if cfg.RetainFailedSandbox {
		t.Fatal("soak must delete failed sandboxes even when other canaries retain them")
	}
	other, err := Load("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if !other.RetainFailedSandbox {
		t.Fatal("soak cleanup policy must not override lifecycle retention")
	}
}

func TestLoadSoakAllowsShortValidationRun(t *testing.T) {
	soakTestEnv(t)
	t.Setenv("CANARY_SOAK_DURATION", "2m")
	t.Setenv("CANARY_SOAK_INTERVAL", "15s")
	t.Setenv("RUN_TIMEOUT", "3m")
	t.Setenv("LOCK_TTL", "5m")
	t.Setenv("RESOURCE_TTL", "5m")
	cfg, err := Load("soak")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SoakDuration != 2*time.Minute || cfg.SoakInterval != 15*time.Second || cfg.RunTimeout != 3*time.Minute || cfg.ResourceTTL != 5*time.Minute {
		t.Fatalf("short run overrides were not preserved: %+v", cfg)
	}
}

func TestLoadSoakRejectsMalformedDurations(t *testing.T) {
	for _, name := range []string{"CANARY_SOAK_DURATION", "CANARY_SOAK_INTERVAL", "RUN_TIMEOUT", "LOCK_TTL", "RESOURCE_TTL"} {
		t.Run(name, func(t *testing.T) {
			soakTestEnv(t)
			t.Setenv(name, "invalid-duration")
			if _, err := Load("soak"); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("malformed %s must be rejected, got %v", name, err)
			}
		})
	}
}

func TestLoadSoakRejectsUnsafeBudgets(t *testing.T) {
	tests := []struct {
		name, key, value, want string
	}{
		{"zero duration", "CANARY_SOAK_DURATION", "0s", "CANARY_SOAK_DURATION"},
		{"negative duration", "CANARY_SOAK_DURATION", "-1s", "CANARY_SOAK_DURATION"},
		{"excessive duration", "CANARY_SOAK_DURATION", "24h1s", "CANARY_SOAK_DURATION"},
		{"zero interval", "CANARY_SOAK_INTERVAL", "0s", "CANARY_SOAK_INTERVAL"},
		{"negative interval", "CANARY_SOAK_INTERVAL", "-1s", "CANARY_SOAK_INTERVAL"},
		{"interval exceeds duration", "CANARY_SOAK_INTERVAL", "6h1s", "CANARY_SOAK_INTERVAL"},
		{"no setup budget", "RUN_TIMEOUT", "6h", "RUN_TIMEOUT"},
		{"unbounded run budget", "RUN_TIMEOUT", "7h1s", "RUN_TIMEOUT"},
		{"lock expires during run", "LOCK_TTL", "1h", "LOCK_TTL"},
		{"lock excludes cleanup grace", "LOCK_TTL", "6h16m44s", "LOCK_TTL"},
		{"resource expires during run", "RESOURCE_TTL", "1h", "RESOURCE_TTL"},
		{"resource excludes cleanup grace", "RESOURCE_TTL", "6h16m44s", "RESOURCE_TTL"},
		{"unbounded resource protection", "RESOURCE_TTL", "7h15m1s", "RESOURCE_TTL"},
		{"privileged preview port", "PREVIEW_PORT", "1023", "PREVIEW_PORT"},
		{"second preview port overflows", "PREVIEW_PORT", "65535", "PREVIEW_PORT"},
		{"zero command timeout", "COMMAND_TIMEOUT", "0s", "timeouts must be positive"},
		{"zero delete timeout", "DELETE_TIMEOUT", "0s", "timeouts must be positive"},
		{"zero preview timeout", "PREVIEW_TIMEOUT", "0s", "timeouts must be positive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			soakTestEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load("soak"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s=%s: error = %v, want %q", tc.key, tc.value, err, tc.want)
			}
		})
	}
}

func TestLoadSoakCleanupBudgetBoundary(t *testing.T) {
	soakTestEnv(t)
	t.Setenv("LOCK_TTL", "6h16m45s")
	t.Setenv("RESOURCE_TTL", "6h16m45s")
	if _, err := Load("soak"); err != nil {
		t.Fatalf("budgets covering run, 45s cleanup, and 1m reporting must be accepted: %v", err)
	}
}

func soakTestEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CANARY_SOAK_DURATION", "CANARY_SOAK_INTERVAL", "RUN_TIMEOUT", "LOCK_TTL", "RESOURCE_TTL",
		"COMMAND_TIMEOUT", "DELETE_TIMEOUT", "PREVIEW_TIMEOUT", "PREVIEW_PORT", "HTTP_TIMEOUT",
		"CANARY_RETAIN_FAILED_SANDBOX", "CANARY_RETAIN_FAILED_SANDBOX_TTL", "CANARY_SLACK_WEBHOOK_URL",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CANARY_TARGET", "staging-us-central1")
	t.Setenv("CANARY_ENVIRONMENT", "staging")
	t.Setenv("CANARY_REGION", "us-central1")
	t.Setenv("GCP_PROJECT_ID", "rayai-dev")
	t.Setenv("API_BASE_URL", "https://api-staging.superserve.ai")
	t.Setenv("PREVIEW_DOMAIN", "staging-sandbox.superserve.ai")
	t.Setenv("CANARY_API_KEY", "test-key")
	t.Setenv("CANARY_RUNTIME", "local")
	t.Setenv("CANARY_METRICS_EXPORTER", "none")
	t.Setenv("CANARY_LOCK_BACKEND", "none")
	t.Setenv("ALLOW_STAGING_MUTATION", "false")
}
