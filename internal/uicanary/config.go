package uicanary

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/superserve-ai/canaries/internal/config"
)

type Config struct {
	BaseConfig             config.Config
	ConsoleURL             string
	Email                  string
	Password               string
	Headless               bool
	ArtifactsDir           string
	StepTimeout            time.Duration
	TerminalTimeout        time.Duration
	VercelProtectionBypass string
}

func LoadConfig(baseCfg config.Config) (Config, error) {
	if baseCfg.Runtime == config.RuntimeCloudRun {
		if strings.TrimSpace(baseCfg.APIKey) == "" || strings.TrimSpace(baseCfg.APIBaseURL) == "" {
			err := errors.New("CANARY_RUNTIME=cloud-run requires CANARY_API_KEY and API_BASE_URL for sandbox ownership tagging")
			log.Error().Err(err).Msg("invalid cloud-run ui canary configuration")
			return Config{}, err
		}
	}

	consoleURL := strings.TrimRight(envDefault("CANARY_UI_CONSOLE_URL", os.Getenv("CANARY_UI_URL")), "/")
	if consoleURL == "" {
		return Config{}, errors.New("console URL is required (set CANARY_UI_CONSOLE_URL)")
	}
	parsedURL, err := url.ParseRequestURI(consoleURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return Config{}, errors.New("CANARY_UI_CONSOLE_URL must be a valid HTTP or HTTPS URL")
	}

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
	vercelBypass := strings.TrimSpace(os.Getenv("CANARY_UI_VERCEL_PROTECTION_BYPASS"))

	return Config{
		BaseConfig:             baseCfg,
		ConsoleURL:             consoleURL,
		Email:                  email,
		Password:               password,
		Headless:               headless,
		ArtifactsDir:           artifactsDir,
		StepTimeout:            stepTimeout,
		TerminalTimeout:        terminalTimeout,
		VercelProtectionBypass: vercelBypass,
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
