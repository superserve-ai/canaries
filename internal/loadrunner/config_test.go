package loadrunner

import (
	"strings"
	"testing"
	"time"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LOAD_TEST_SCENARIO", "LOAD_TEST_SCHEDULED", "LOAD_TEST_TIME_ZONE",
		"LOAD_TEST_DAILY_WINDOW_START", "LOAD_TEST_DAILY_WINDOW_END",
		"CANARY_LOCK_BACKEND", "LOCK_BUCKET", "CANARY_LOCK_FILE", "LOAD_TEST_LOCK_TTL",
		"LOAD_TEST_BENCHMARK_GUARD", "LOAD_TEST_GLOBAL_LOCK", "LOAD_TEST_EXTERNAL_BENCHMARK_GUARD",
		"CANARY_SLACK_WEBHOOK_URL", "LOAD_TEST_PRODUCTION_OPT_IN", "LOAD_TEST_ALLOW_PRODUCTION",
	} {
		t.Setenv(name, "")
	}
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
		{name: "dangling query", env: "API_BASE_URL", value: "https://api-staging.superserve.ai?"},
		{name: "dangling fragment", env: "API_BASE_URL", value: "https://api-staging.superserve.ai#"},
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

func TestLoadScenarioModes(t *testing.T) {
	for _, tt := range []struct {
		name, value, want string
		valid             bool
	}{
		{name: "default create", want: "create", valid: true},
		{name: "explicit create", value: "create", want: "create", valid: true},
		{name: "full lifecycle", value: "lifecycle", want: "lifecycle", valid: true},
		{name: "unknown scenario", value: "pause"},
		{name: "case sensitive", value: "LIFECYCLE"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LOAD_TEST_SCENARIO", tt.value)
			cfg, err := Load()
			if !tt.valid {
				if err == nil || !strings.Contains(err.Error(), "LOAD_TEST_SCENARIO") {
					t.Fatalf("Load scenario %q returned %v, want scenario error", tt.value, err)
				}
				return
			}
			if err != nil || cfg.Scenario != tt.want || cfg.Scheduled {
				t.Fatalf("Load scenario %q = scenario %q scheduled %v, error %v", tt.value, cfg.Scenario, cfg.Scheduled, err)
			}
		})
	}
}

func TestLoadAcceptsActualProductionRoutingAndLegacyAliases(t *testing.T) {
	for _, tt := range []struct {
		name, region, api, preview string
	}{
		{"east canonical", "us-east4", "https://api.superserve.ai", "sandbox.superserve.ai"},
		{"east legacy preview", "us-east4", "https://api.superserve.ai", "use-sandbox.superserve.ai"},
		{"west canonical", "us-west2", "https://api-usw.superserve.ai", "usw-sandbox.superserve.ai"},
		{"west legacy API", "us-west2", "https://usw-api.superserve.ai", "usw-sandbox.superserve.ai"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setProductionEnv(t, tt.region, tt.api, tt.preview)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load rejected supported route: %v", err)
			}
			if cfg.APIBaseURL != tt.api || cfg.PreviewDomain != tt.preview || cfg.Target != "production-"+tt.region {
				t.Fatalf("route changed: API=%q preview=%q target=%q", cfg.APIBaseURL, cfg.PreviewDomain, cfg.Target)
			}
		})
	}
}

func TestLoadRejectsUnsafeAPIURLWithoutExposingCredentials(t *testing.T) {
	for _, tt := range []struct{ name, url string }{
		{"username", "https://private-user@api-staging.superserve.ai"},
		{"password", "https://private-user:private-password@api-staging.superserve.ai"},
		{"encoded userinfo", "https://private%2Duser:private%2Dpassword@api-staging.superserve.ai"},
		{"default port", "https://api-staging.superserve.ai:443"},
		{"alternate port", "https://api-staging.superserve.ai:8443"},
		{"empty port", "https://api-staging.superserve.ai:"},
		{"invalid port", "https://api-staging.superserve.ai:invalid"},
		{"host suffix", "https://api-staging.superserve.ai.attacker.example"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("API_BASE_URL", tt.url)
			_, err := Load()
			if err == nil {
				t.Fatal("Load accepted an unsafe API URL")
			}
			for _, private := range []string{"private-user", "private-password", "private%2Duser", "private%2Dpassword"} {
				if strings.Contains(err.Error(), private) {
					t.Fatal("routing error exposed URL credentials")
				}
			}
		})
	}
}

func setScheduledLoadEnv(t *testing.T) {
	t.Helper()
	setRequiredEnv(t)
	t.Setenv("LOAD_TEST_SCENARIO", "lifecycle")
	t.Setenv("LOAD_TEST_SCHEDULED", "true")
	t.Setenv("LOAD_TEST_OPERATIONS", "1000")
	t.Setenv("LOAD_TEST_CONCURRENCY", "100")
	t.Setenv("LOAD_TEST_RUN_TIMEOUT", "20m")
	t.Setenv("DELETE_TIMEOUT", "2m")
	t.Setenv("LOAD_TEST_RESOURCE_TTL", "2h")
	t.Setenv("LOAD_TEST_LOCK_TTL", "45m")
	t.Setenv("CANARY_LOCK_BACKEND", "gcs")
	t.Setenv("LOCK_BUCKET", "shared-canary-locks")
	t.Setenv("CANARY_SLACK_WEBHOOK_URL", "https://hooks.slack.example/services/test-webhook")
	t.Setenv("LOAD_TEST_DAILY_WINDOW_START", "07:45")
	t.Setenv("LOAD_TEST_DAILY_WINDOW_END", "08:15")
}

func TestLoadScheduledDefaultsEnableAllGuards(t *testing.T) {
	setScheduledLoadEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load rejected scheduled configuration: %v", err)
	}
	if !cfg.Scheduled || cfg.Scenario != "lifecycle" || !cfg.BenchmarkGuard || !cfg.GlobalLock || !cfg.ExternalBenchmarkGuard || cfg.LockBackend != "gcs" || cfg.LockBucket == "" || cfg.SlackWebhookURL == "" {
		t.Fatal("scheduled defaults omitted a required coordination or reporting guard")
	}
	if cfg.Operations != 1000 || cfg.Concurrency != 100 || cfg.RunTimeout != 20*time.Minute || cfg.DeleteTimeout != 2*time.Minute || cfg.TimeZone != "America/Los_Angeles" {
		t.Fatal("scheduled configuration did not preserve its configured load and timing bounds")
	}
}

func TestLoadScheduledRequiresEverySafetyPrerequisite(t *testing.T) {
	for _, tt := range []struct{ name, env, value string }{
		{"full lifecycle", "LOAD_TEST_SCENARIO", "create"},
		{"shared GCS coordination", "CANARY_LOCK_BACKEND", "none"},
		{"lock bucket", "LOCK_BUCKET", ""},
		{"benchmark guard", "LOAD_TEST_BENCHMARK_GUARD", "false"},
		{"global lock", "LOAD_TEST_GLOBAL_LOCK", "false"},
		{"external benchmark guard", "LOAD_TEST_EXTERNAL_BENCHMARK_GUARD", "false"},
		{"summary webhook", "CANARY_SLACK_WEBHOOK_URL", ""},
		{"window start", "LOAD_TEST_DAILY_WINDOW_START", ""},
		{"window end", "LOAD_TEST_DAILY_WINDOW_END", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setScheduledLoadEnv(t)
			t.Setenv(tt.env, tt.value)
			if _, err := Load(); err == nil {
				t.Fatalf("scheduled configuration accepted missing %s", tt.name)
			}
		})
	}
}

func TestLoadScheduledRegionalDailyWindows(t *testing.T) {
	for _, tt := range []struct{ region, api, preview, start, end string }{
		{"us-east4", "https://api.superserve.ai", "sandbox.superserve.ai", "07:45", "08:15"},
		{"us-west2", "https://api-usw.superserve.ai", "usw-sandbox.superserve.ai", "08:15", "08:45"},
	} {
		t.Run(tt.region, func(t *testing.T) {
			setScheduledLoadEnv(t)
			t.Setenv("CANARY_ENVIRONMENT", "production")
			t.Setenv("CANARY_REGION", tt.region)
			t.Setenv("CANARY_TARGET", "production-"+tt.region)
			t.Setenv("API_BASE_URL", tt.api)
			t.Setenv("PREVIEW_DOMAIN", tt.preview)
			t.Setenv("LOAD_TEST_PRODUCTION_OPT_IN", "true")
			t.Setenv("LOAD_TEST_DAILY_WINDOW_START", tt.start)
			t.Setenv("LOAD_TEST_DAILY_WINDOW_END", tt.end)
			cfg, err := Load()
			if err != nil || cfg.DailyWindowStart != tt.start || cfg.DailyWindowEnd != tt.end {
				t.Fatalf("Load window %s-%s returned %v", tt.start, tt.end, err)
			}
		})
	}
}

func TestLoadScheduledWindowCoversRuntimeCleanupAndReporting(t *testing.T) {
	for _, tt := range []struct {
		name, end string
		valid     bool
	}{
		{"one minute short", "08:07", false},
		{"exact twenty-three minute budget", "08:08", true},
		{"one minute spare", "08:09", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setScheduledLoadEnv(t)
			t.Setenv("LOAD_TEST_DAILY_WINDOW_END", tt.end)
			_, err := Load()
			if (err == nil) != tt.valid {
				t.Fatalf("Load window 07:45-%s returned %v, want valid=%v", tt.end, err, tt.valid)
			}
		})
	}
}

func TestLoadLockAndResourceTTLCoverWholeExecutionBudget(t *testing.T) {
	for _, env := range []string{"LOAD_TEST_LOCK_TTL", "LOAD_TEST_RESOURCE_TTL"} {
		for _, tt := range []struct {
			name, ttl string
			valid     bool
		}{
			{"one nanosecond short", "22m59.999999999s", false},
			{"exact budget", "23m", true},
			{"one nanosecond spare", "23m0.000000001s", true},
		} {
			t.Run(env+"/"+tt.name, func(t *testing.T) {
				setScheduledLoadEnv(t)
				t.Setenv(env, tt.ttl)
				_, err := Load()
				if (err == nil) != tt.valid {
					t.Fatalf("Load %s=%s returned %v, want valid=%v", env, tt.ttl, err, tt.valid)
				}
			})
		}
	}
}

func TestLoadRejectsInvalidLockConfiguration(t *testing.T) {
	for _, tt := range []struct{ name, backend, file, bucket, ttl string }{
		{name: "unsupported backend", backend: "redis"},
		{name: "file without path", backend: "file"},
		{name: "GCS without bucket", backend: "gcs"},
		{name: "lock lease above maximum", backend: "gcs", bucket: "shared-locks", ttl: "2h0.000000001s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("CANARY_LOCK_BACKEND", tt.backend)
			t.Setenv("CANARY_LOCK_FILE", tt.file)
			t.Setenv("LOCK_BUCKET", tt.bucket)
			if tt.ttl != "" {
				t.Setenv("LOAD_TEST_LOCK_TTL", tt.ttl)
			}
			if _, err := Load(); err == nil {
				t.Fatal("Load accepted an incomplete or unbounded lock configuration")
			}
		})
	}
}

func TestLoadRejectsMalformedSafetyBooleans(t *testing.T) {
	for _, name := range []string{"LOAD_TEST_SCHEDULED", "LOAD_TEST_BENCHMARK_GUARD", "LOAD_TEST_GLOBAL_LOCK", "LOAD_TEST_EXTERNAL_BENCHMARK_GUARD"} {
		t.Run(name, func(t *testing.T) {
			setScheduledLoadEnv(t)
			t.Setenv(name, "yes-please")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Load returned %v, want invalid %s error", err, name)
			}
		})
	}
}

func TestLoadRejectsMalformedScheduledWindowAndTimeZone(t *testing.T) {
	for _, tt := range []struct{ name, start, end, zone string }{
		{name: "unknown time zone", start: "07:45", end: "08:15", zone: "America/Not_A_Zone"},
		{name: "numeric offset is not a zone", start: "07:45", end: "08:15", zone: "UTC-07:00"},
		{name: "late start text", start: "late", end: "08:15"},
		{name: "late end text", start: "07:45", end: "late"},
		{name: "unbounded hour", start: "25:00", end: "26:00"},
		{name: "unbounded minute", start: "07:60", end: "08:30"},
		{name: "unmapped seconds", start: "07:45:00", end: "08:15:00"},
		{name: "unpadded start hour", start: "7:45", end: "08:15"},
		{name: "unpadded end hour", start: "07:45", end: "8:15"},
		{name: "same start and end", start: "07:45", end: "07:45"},
		{name: "reversed window", start: "08:15", end: "07:45"},
		{name: "overnight window", start: "23:45", end: "00:15"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setScheduledLoadEnv(t)
			t.Setenv("LOAD_TEST_DAILY_WINDOW_START", tt.start)
			t.Setenv("LOAD_TEST_DAILY_WINDOW_END", tt.end)
			if tt.zone != "" {
				t.Setenv("LOAD_TEST_TIME_ZONE", tt.zone)
			}
			if _, err := Load(); err == nil {
				t.Fatal("Load accepted an invalid scheduled time zone or same-day window")
			}
		})
	}
}

func TestLoadRejectsUnsafeSummaryWebhook(t *testing.T) {
	for _, url := range []string{
		"http://hooks.slack.example/services/test",
		"https://private-user:private-password@hooks.slack.example/services/test",
		"https://hooks.slack.example/services/test#fragment",
		"https:///services/test",
	} {
		t.Run(url, func(t *testing.T) {
			setScheduledLoadEnv(t)
			t.Setenv("CANARY_SLACK_WEBHOOK_URL", url)
			_, err := Load()
			if err == nil || strings.Contains(err.Error(), "private-password") {
				t.Fatal("Load accepted unsafe webhook or exposed its credentials")
			}
		})
	}
}
