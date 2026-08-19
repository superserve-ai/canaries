package loadrunner

import (
	"strings"
	"testing"
	"time"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CANARY_ENVIRONMENT", "staging")
	t.Setenv("CANARY_REGION", "us-central1")
	t.Setenv("CANARY_TARGET", stagingTarget)
	t.Setenv("API_BASE_URL", stagingAPIBaseURL)
	t.Setenv("PREVIEW_DOMAIN", stagingPreviewDomain)
	t.Setenv("CANARY_API_KEY", "test-key")
}

func setProductionEnv(t *testing.T, region, apiBaseURL, previewDomain string) {
	t.Helper()
	setRequiredEnv(t)
	t.Setenv("CANARY_ENVIRONMENT", "production")
	t.Setenv("CANARY_REGION", region)
	t.Setenv("CANARY_TARGET", "production-"+region)
	t.Setenv("API_BASE_URL", apiBaseURL)
	t.Setenv("PREVIEW_DOMAIN", previewDomain)
	t.Setenv("CANARY_API_KEY", "production-key")
	t.Setenv("LOAD_TEST_PRODUCTION_OPT_IN", "true")
}

func TestLoadDefaultsEnvironmentToStaging(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("CANARY_ENVIRONMENT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	if cfg.Environment != "staging" {
		t.Fatalf("Environment = %q, want staging", cfg.Environment)
	}
}

func TestLoadUsesStandardCanaryAPIKey(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("CANARY_API_KEY_STAGING", "environment-specific-key")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	if cfg.APIKey != "test-key" {
		t.Fatalf("APIKey = %q, want CANARY_API_KEY", cfg.APIKey)
	}
}

func TestLoadFallsBackToEnvironmentSpecificAPIKey(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("CANARY_API_KEY", "")
	t.Setenv("CANARY_API_KEY_STAGING", "staging-key")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	if cfg.APIKey != "staging-key" {
		t.Fatalf("APIKey = %q, want environment-specific fallback", cfg.APIKey)
	}
}

func TestLoadAcceptsKnownProductionTargets(t *testing.T) {
	tests := []struct {
		region        string
		apiBaseURL    string
		previewDomain string
	}{
		{region: "us-central1", apiBaseURL: "https://api.superserve.ai", previewDomain: "sandbox.superserve.ai"},
		{region: "us-east4", apiBaseURL: "https://api.superserve.ai", previewDomain: "use-sandbox.superserve.ai"},
		{region: "us-west2", apiBaseURL: "https://usw-api.superserve.ai", previewDomain: "usw-sandbox.superserve.ai"},
	}
	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			setProductionEnv(t, tt.region, tt.apiBaseURL, tt.previewDomain)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load rejected production target: %v", err)
			}
			if cfg.Environment != "production" || cfg.Region != tt.region || cfg.Target != "production-"+tt.region {
				t.Fatalf("unexpected production config: %+v", cfg)
			}
		})
	}
}

func TestLoadRejectsProductionWithoutOptIn(t *testing.T) {
	setProductionEnv(t, "us-central1", "https://api.superserve.ai", "sandbox.superserve.ai")
	t.Setenv("LOAD_TEST_PRODUCTION_OPT_IN", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected production without explicit opt-in to be rejected")
	}
	if !strings.Contains(err.Error(), "LOAD_TEST_PRODUCTION_OPT_IN=true") {
		t.Fatalf("unexpected production guard error: %v", err)
	}
}

func TestLoadRejectsConflictingProductionGuards(t *testing.T) {
	setProductionEnv(t, "us-central1", "https://api.superserve.ai", "sandbox.superserve.ai")
	t.Setenv("LOAD_TEST_ALLOW_PRODUCTION", "false")
	if _, err := Load(); err == nil {
		t.Fatal("expected conflicting production guards to be rejected")
	}
}

func TestLoadRejectsUnsupportedEnvironment(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("CANARY_ENVIRONMENT", "development")
	t.Setenv("CANARY_TARGET", "development-us-central1")
	if _, err := Load(); err == nil {
		t.Fatal("expected unsupported environment to be rejected")
	}
}

func TestLoadRejectsUnsupportedRegion(t *testing.T) {
	setProductionEnv(t, "europe-west1", "https://api.superserve.ai", "sandbox.superserve.ai")
	if _, err := Load(); err == nil {
		t.Fatal("expected unsupported production region to be rejected")
	}
}

func TestLoadRejectsTargetThatDoesNotMatchEnvironmentAndRegion(t *testing.T) {
	setProductionEnv(t, "us-central1", "https://api.superserve.ai", "sandbox.superserve.ai")
	t.Setenv("CANARY_TARGET", "production-us-east4")
	if _, err := Load(); err == nil {
		t.Fatal("expected mismatched target to be rejected")
	}
}

func TestLoadRejectsRoutingHostMismatch(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		value string
	}{
		{name: "api host", env: "API_BASE_URL", value: "https://api-staging-alt.superserve.ai"},
		{name: "api path", env: "API_BASE_URL", value: "https://api-staging.superserve.ai/sandboxes"},
		{name: "insecure api", env: "API_BASE_URL", value: "http://api-staging.superserve.ai"},
		{name: "preview host", env: "PREVIEW_DOMAIN", value: "staging.attacker.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tt.env, tt.value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted invalid %s", tt.env)
			}
		})
	}
}

func TestLoadRejectsOperationCountAboveHardCeiling(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOAD_TEST_OPERATIONS", "10001")
	if _, err := Load(); err == nil {
		t.Fatal("expected operation count above hard ceiling to be rejected")
	}
}

func TestLoadRejectsConcurrencyAboveOperations(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOAD_TEST_OPERATIONS", "10")
	t.Setenv("LOAD_TEST_CONCURRENCY", "11")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid concurrency")
	}
}

func TestLoadAcceptsBoundedConfig(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOAD_TEST_OPERATIONS", "100")
	t.Setenv("LOAD_TEST_CONCURRENCY", "20")
	t.Setenv("LOAD_TEST_RUN_ID", "run-123")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	if cfg.RunID != "run-123" || cfg.Operations != 100 || cfg.Concurrency != 20 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadAcceptsConfiguredActiveTimeout(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOAD_TEST_ACTIVE_TIMEOUT", "17s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	if cfg.ActiveTimeout != 17*time.Second {
		t.Fatalf("ActiveTimeout = %s, want 17s", cfg.ActiveTimeout)
	}
}

func TestLoadRunIDIsUniqueAndSafeForSandboxNames(t *testing.T) {
	setRequiredEnv(t)
	first, err := Load()
	if err != nil {
		t.Fatalf("first Load returned %v", err)
	}
	second, err := Load()
	if err != nil {
		t.Fatalf("second Load returned %v", err)
	}
	if first.RunID == second.RunID {
		t.Fatalf("generated run IDs collided: %q", first.RunID)
	}
	for _, id := range []string{first.RunID, second.RunID} {
		if len(id) > maxRunIDLength || strings.Trim(id, "-") != id {
			t.Fatalf("generated run ID is not bounded: %q", id)
		}
	}

	t.Setenv("LOAD_TEST_RUN_ID", "  Very unusual/run ID with spaces and a deliberately long suffix that exceeds backend limits  ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with unusual run ID returned %v", err)
	}
	if len(cfg.RunID) > maxRunIDLength || strings.Trim(cfg.RunID, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		t.Fatalf("unusual run ID was not made backend-safe: %q", cfg.RunID)
	}
}

func TestSafeRunIDPreservesDistinctNormalizedInputs(t *testing.T) {
	first := safeRunID("run/a")
	second := safeRunID("run-a")
	if first == second {
		t.Fatalf("distinct run IDs collided after normalization: %q", first)
	}
}

func TestLoadRejectsDurationsAboveSafetyBounds(t *testing.T) {
	tests := []struct {
		name string
		env  string
		max  time.Duration
	}{
		{name: "run timeout", env: "LOAD_TEST_RUN_TIMEOUT", max: maxRunTimeout},
		{name: "resource ttl", env: "LOAD_TEST_RESOURCE_TTL", max: maxResourceTTL},
		{name: "poll interval", env: "POLL_INTERVAL", max: maxPollInterval},
		{name: "http timeout", env: "HTTP_TIMEOUT", max: maxHTTPTimeout},
		{name: "command timeout", env: "COMMAND_TIMEOUT", max: maxCommandTimeout},
		{name: "active timeout", env: "LOAD_TEST_ACTIVE_TIMEOUT", max: maxActiveTimeout},
		{name: "delete timeout", env: "DELETE_TIMEOUT", max: maxDeleteTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tt.env, (tt.max + time.Nanosecond).String())
			if _, err := Load(); err == nil {
				t.Fatalf("expected %s above %s to be rejected", tt.env, tt.max)
			}
		})
	}
}

func TestLoadRejectsSubsecondCommandTimeout(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("COMMAND_TIMEOUT", "500ms")
	if _, err := Load(); err == nil {
		t.Fatal("expected sub-second command timeout to be rejected")
	}
}
