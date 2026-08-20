package uicanary

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/superserve-ai/canaries/internal/config"
)

type Config struct {
	BaseConfig      config.Config
	ConsoleURL      string
	Email           string
	Password        string
	Headless        bool
	ArtifactsDir    string
	StepTimeout     time.Duration
	TerminalTimeout time.Duration
}

func LoadConfig(baseCfg config.Config) (Config, error) {
	consoleURL := strings.TrimRight(envDefault("CANARY_UI_URL", "https://console.superserve.ai"), "/")

	email := envDefault("CANARY_UI_EMAIL", os.Getenv("CANARY_UI_USERNAME"))
	password := os.Getenv("CANARY_UI_PASSWORD")

	if email == "" {
		return Config{}, errors.New("CANARY_UI_EMAIL is required")
	}
	if password == "" {
		return Config{}, errors.New("CANARY_UI_PASSWORD is required")
	}

	headless := getenvBool("CANARY_UI_HEADLESS", true)
	artifactsDir := envDefault("CANARY_UI_ARTIFACTS_DIR", "/tmp/ui-canary-artifacts")
	stepTimeout := getenvDuration("CANARY_UI_STEP_TIMEOUT", 45*time.Second)
	terminalTimeout := getenvDuration("CANARY_UI_TERMINAL_TIMEOUT", 30*time.Second)

	return Config{
		BaseConfig:      baseCfg,
		ConsoleURL:      consoleURL,
		Email:           email,
		Password:        password,
		Headless:        headless,
		ArtifactsDir:    artifactsDir,
		StepTimeout:     stepTimeout,
		TerminalTimeout: terminalTimeout,
	}, nil
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
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
