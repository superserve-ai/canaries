package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModeLifecycle Mode = "lifecycle"
	ModeJanitor   Mode = "janitor"
)

type Config struct {
	Mode             Mode
	Target           string
	Environment      string
	Region           string
	ProjectID        string
	APIBaseURL       string
	PreviewDomain    string
	APIKey           string
	RunTimeout       time.Duration
	ResourceTTL      time.Duration
	PollInterval     time.Duration
	LockTTL          time.Duration
	LockBucket       string
	HTTPTimeout      time.Duration
	Metrics          metricsConfig
	ManualStaging    bool
	PreviewPort      int
	CommandTimeout   time.Duration
	DeleteTimeout    time.Duration
	PreviewTimeout   time.Duration
	JanitorThreshold time.Duration
}

type metricsConfig struct {
	ServiceName string
	Environment string
}

func Load(rawMode string) (Config, error) {
	mode := Mode(strings.TrimSpace(rawMode))
	if mode != ModeLifecycle && mode != ModeJanitor {
		return Config{}, fmt.Errorf("invalid mode %q", rawMode)
	}

	cfg := Config{
		Mode:             mode,
		Target:           os.Getenv("CANARY_TARGET"),
		Environment:      os.Getenv("CANARY_ENVIRONMENT"),
		Region:           os.Getenv("CANARY_REGION"),
		ProjectID:        os.Getenv("GCP_PROJECT_ID"),
		APIBaseURL:       os.Getenv("API_BASE_URL"),
		PreviewDomain:    os.Getenv("PREVIEW_DOMAIN"),
		APIKey:           os.Getenv("CANARY_API_KEY"),
		LockBucket:       os.Getenv("LOCK_BUCKET"),
		ManualStaging:    getenvBool("MANUAL_STAGING_OPT_IN", false),
		PreviewPort:      getenvInt("PREVIEW_PORT", 18080),
		RunTimeout:       getenvDuration("RUN_TIMEOUT", 4*time.Minute),
		ResourceTTL:      getenvDuration("RESOURCE_TTL", 1*time.Hour),
		PollInterval:     getenvDuration("POLL_INTERVAL", 3*time.Second),
		LockTTL:          getenvDuration("LOCK_TTL", 10*time.Minute),
		HTTPTimeout:      getenvDuration("HTTP_TIMEOUT", 30*time.Second),
		CommandTimeout:   getenvDuration("COMMAND_TIMEOUT", 45*time.Second),
		DeleteTimeout:    getenvDuration("DELETE_TIMEOUT", 45*time.Second),
		PreviewTimeout:   getenvDuration("PREVIEW_TIMEOUT", 20*time.Second),
		JanitorThreshold: getenvDuration("JANITOR_STALE_THRESHOLD", 1*time.Hour),
		Metrics: metricsConfig{
			ServiceName: envDefault("OTEL_SERVICE_NAME", "superserve-api-canary"),
			Environment: envDefault("OTEL_ENVIRONMENT", os.Getenv("CANARY_ENVIRONMENT")),
		},
	}

	var missing []string
	for _, item := range []struct {
		value string
		name  string
	}{
		{cfg.Target, "CANARY_TARGET"},
		{cfg.Environment, "CANARY_ENVIRONMENT"},
		{cfg.Region, "CANARY_REGION"},
		{cfg.ProjectID, "GCP_PROJECT_ID"},
		{cfg.APIBaseURL, "API_BASE_URL"},
		{cfg.PreviewDomain, "PREVIEW_DOMAIN"},
		{cfg.APIKey, "CANARY_API_KEY"},
	} {
		if strings.TrimSpace(item.value) == "" {
			missing = append(missing, item.name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if mode == ModeLifecycle && cfg.LockBucket == "" {
		missing = append(missing, "LOCK_BUCKET")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if cfg.Environment == "staging" && !cfg.ManualStaging && os.Getenv("ALLOW_STAGING_MUTATION") == "true" {
		return Config{}, errors.New("manual staging runs require MANUAL_STAGING_OPT_IN=true")
	}
	return cfg, nil
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
