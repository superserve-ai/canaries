package uicanary

import (
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/config"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("CANARY_UI_EMAIL", "test@superserve.ai")
	t.Setenv("CANARY_UI_PASSWORD", "secret123")
	t.Setenv("CANARY_UI_URL", "")
	t.Setenv("UI_CANARY_BASE_URL", "")
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

	if cfg.ConsoleURL != "https://console.superserve.ai" {
		t.Errorf("expected default console url https://console.superserve.ai, got %s", cfg.ConsoleURL)
	}
	if cfg.Email != "test@superserve.ai" || cfg.Password != "secret123" {
		t.Errorf("unexpected credentials: %s / %s", cfg.Email, cfg.Password)
	}
	if !cfg.Headless {
		t.Errorf("expected headless true by default")
	}
}

func TestLoadConfigCustom(t *testing.T) {
	t.Setenv("CANARY_UI_URL", "http://localhost:3000/")
	t.Setenv("CANARY_UI_EMAIL", "custom@superserve.ai")
	t.Setenv("CANARY_UI_PASSWORD", "secret123")
	t.Setenv("CANARY_UI_HEADLESS", "false")
	t.Setenv("CANARY_UI_STEP_TIMEOUT", "20s")

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
}

func TestLoadConfigUsernameFallback(t *testing.T) {
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
	t.Setenv("CANARY_UI_EMAIL", "")
	t.Setenv("CANARY_UI_USERNAME", "")
	t.Setenv("CANARY_UI_PASSWORD", "")
	t.Setenv("UI_CANARY_PASSWORD", "")

	base := config.Config{}

	// No credentials
	_, err := LoadConfig(base)
	if err == nil {
		t.Errorf("expected error when credentials are missing")
	}

	// Missing password
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
