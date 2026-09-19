package uicanary

import (
	"strings"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/config"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("CANARY_UI_CONSOLE_URL", "https://console-staging.superserve.ai")
	t.Setenv("CANARY_UI_EMAIL", "test@superserve.ai")
	t.Setenv("CANARY_UI_PASSWORD", "secret123")
	t.Setenv("CANARY_UI_HEADLESS", "")

	base := config.Config{
		Environment: "staging",
		Region:      "us-central1",
		Target:      "staging-us-central1",
		RunTimeout:  3 * time.Minute,
	}

	cfg, err := LoadConfig(base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ConsoleURL != "https://console-staging.superserve.ai" {
		t.Errorf("expected console url https://console-staging.superserve.ai, got %s", cfg.ConsoleURL)
	}
	if cfg.Email != "test@superserve.ai" || cfg.Password != "secret123" {
		t.Errorf("unexpected credentials: %s / %s", cfg.Email, cfg.Password)
	}
	if !cfg.Headless {
		t.Errorf("expected headless true by default")
	}
	if cfg.VercelProtectionBypass != "" {
		t.Errorf("expected empty VercelProtectionBypass by default, got %q", cfg.VercelProtectionBypass)
	}
}

func TestLoadConfigCustom(t *testing.T) {
	t.Setenv("CANARY_UI_URL", "http://localhost:3000/")
	t.Setenv("CANARY_UI_EMAIL", "custom@superserve.ai")
	t.Setenv("CANARY_UI_PASSWORD", "secret123")
	t.Setenv("CANARY_UI_HEADLESS", "false")
	t.Setenv("CANARY_UI_STEP_TIMEOUT", "20s")
	t.Setenv("CANARY_UI_VERCEL_PROTECTION_BYPASS", "secret-bypass-token")

	base := config.Config{}
	cfg, err := LoadConfig(base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ConsoleURL != "http://localhost:3000" {
		t.Errorf("expected trailing slash trimmed, got %s", cfg.ConsoleURL)
	}
	if cfg.Email != "custom@superserve.ai" || cfg.Password != "secret123" {
		t.Errorf("unexpected credentials: %s / %s", cfg.Email, cfg.Password)
	}
	if cfg.Headless {
		t.Errorf("expected headless false")
	}
	if cfg.StepTimeout != 20*time.Second {
		t.Errorf("expected 20s step timeout, got %v", cfg.StepTimeout)
	}
	if cfg.VercelProtectionBypass != "secret-bypass-token" {
		t.Errorf("expected secret-bypass-token, got %q", cfg.VercelProtectionBypass)
	}
}

func TestLoadConfigUsernameFallback(t *testing.T) {
	t.Setenv("CANARY_UI_CONSOLE_URL", "http://localhost:3000")
	t.Setenv("CANARY_UI_EMAIL", "")
	t.Setenv("CANARY_UI_USERNAME", "legacy@superserve.ai")
	t.Setenv("CANARY_UI_PASSWORD", "secret123")

	base := config.Config{}
	cfg, err := LoadConfig(base)
	if err != nil {
		t.Fatalf("unexpected error when using username fallback: %v", err)
	}
	if cfg.Email != "legacy@superserve.ai" {
		t.Errorf("expected fallback username to be loaded as email, got %s", cfg.Email)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	t.Setenv("CANARY_UI_CONSOLE_URL", "")
	t.Setenv("CANARY_UI_URL", "")
	t.Setenv("CANARY_UI_EMAIL", "")
	t.Setenv("CANARY_UI_USERNAME", "")
	t.Setenv("CANARY_UI_PASSWORD", "")

	base := config.Config{}

	// Missing console URL
	_, err := LoadConfig(base)
	if err == nil || err.Error() != "console URL is required (set CANARY_UI_CONSOLE_URL)" {
		t.Errorf("expected 'console URL is required (set CANARY_UI_CONSOLE_URL)', got %v", err)
	}

	// Missing password
	t.Setenv("CANARY_UI_CONSOLE_URL", "http://localhost:3000")
	t.Setenv("CANARY_UI_EMAIL", "user@test.com")
	_, err = LoadConfig(base)
	if err == nil {
		t.Errorf("expected error when password is missing")
	}

	// Missing email
	t.Setenv("CANARY_UI_EMAIL", "")
	t.Setenv("CANARY_UI_PASSWORD", "secret")
	_, err = LoadConfig(base)
	if err == nil {
		t.Errorf("expected error when email is missing")
	}
}

func TestValidateConsoleURL(t *testing.T) {
	base := config.Config{}
	t.Setenv("CANARY_UI_EMAIL", "user@test.com")
	t.Setenv("CANARY_UI_PASSWORD", "secret123")

	validCases := []string{
		"http://localhost:3000",
		"https://console-staging.superserve.ai",
		"https://console.superserve.ai/path",
	}
	for _, url := range validCases {
		t.Setenv("CANARY_UI_CONSOLE_URL", url)
		if _, err := LoadConfig(base); err != nil {
			t.Errorf("expected %q to be valid, got %v", url, err)
		}
	}

	invalidCases := []string{
		"ftp://example.com",
		"not-a-url",
		"/just/a/path",
		"://missing-scheme",
		"http://",
	}
	for _, url := range invalidCases {
		t.Setenv("CANARY_UI_CONSOLE_URL", url)
		_, err := LoadConfig(base)
		if err == nil || err.Error() != "CANARY_UI_CONSOLE_URL must be a valid HTTP or HTTPS URL" {
			t.Errorf("expected invalid URL error for %q, got %v", url, err)
		}
	}

	for _, url := range []string{"", "   "} {
		t.Setenv("CANARY_UI_CONSOLE_URL", url)
		_, err := LoadConfig(base)
		if err == nil || err.Error() != "console URL is required (set CANARY_UI_CONSOLE_URL)" {
			t.Errorf("expected required URL error for %q, got %v", url, err)
		}
	}
}

func TestLoadConfigCloudRunTaggingCredentials(t *testing.T) {
	t.Setenv("CANARY_UI_CONSOLE_URL", "https://console-staging.superserve.ai")
	t.Setenv("CANARY_UI_EMAIL", "test@superserve.ai")
	t.Setenv("CANARY_UI_PASSWORD", "secret123")

	// Missing both
	cloudRunBase := config.Config{Runtime: config.RuntimeCloudRun}
	_, err := LoadConfig(cloudRunBase)
	if err == nil || !strings.Contains(err.Error(), "requires CANARY_API_KEY and API_BASE_URL") {
		t.Fatalf("expected error requiring API key and base URL, got: %v", err)
	}

	// Missing API Key
	cloudRunBase.APIBaseURL = "https://api-staging.superserve.ai"
	_, err = LoadConfig(cloudRunBase)
	if err == nil || !strings.Contains(err.Error(), "requires CANARY_API_KEY and API_BASE_URL") {
		t.Fatalf("expected error requiring API key, got: %v", err)
	}

	// Missing API Base URL
	cloudRunBase.APIBaseURL = ""
	cloudRunBase.APIKey = "key_123"
	_, err = LoadConfig(cloudRunBase)
	if err == nil || !strings.Contains(err.Error(), "requires CANARY_API_KEY and API_BASE_URL") {
		t.Fatalf("expected error requiring API base URL, got: %v", err)
	}

	// Both present
	cloudRunBase.APIBaseURL = "https://api-staging.superserve.ai"
	cfg, err := LoadConfig(cloudRunBase)
	if err != nil {
		t.Fatalf("unexpected error with valid cloud run config: %v", err)
	}
	if cfg.BaseConfig.APIKey != "key_123" {
		t.Errorf("expected APIKey key_123, got: %s", cfg.BaseConfig.APIKey)
	}
}
