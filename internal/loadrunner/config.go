package loadrunner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const MaxOperations = 10_000

const maxRunIDLength = 48
const runIDHashLength = 12

const (
	stagingTarget        = "staging-us-central1"
	stagingAPIBaseURL    = "https://api-staging.superserve.ai"
	stagingPreviewDomain = "staging-sandbox.superserve.ai"
)

const (
	maxRunTimeout     = time.Hour
	maxResourceTTL    = 24 * time.Hour
	maxPollInterval   = time.Minute
	maxHTTPTimeout    = 5 * time.Minute
	maxCommandTimeout = 5 * time.Minute
	maxActiveTimeout  = 10 * time.Minute
	maxDeleteTimeout  = 5 * time.Minute
)

type Config struct {
	Scenario               string
	Environment            string
	ProductionOptIn        bool
	Region                 string
	Target                 string
	APIBaseURL             string
	PreviewDomain          string
	APIKey                 string
	Template               string
	RunID                  string
	WorkerID               string
	Operations             int
	Concurrency            int
	RunTimeout             time.Duration
	ResourceTTL            time.Duration
	PollInterval           time.Duration
	HTTPTimeout            time.Duration
	CommandTimeout         time.Duration
	ActiveTimeout          time.Duration
	DeleteTimeout          time.Duration
	SlackWebhookURL        string
	Scheduled              bool
	TimeZone               string
	DailyWindowStart       string
	DailyWindowEnd         string
	LockBackend            string
	LockBucket             string
	LockFile               string
	LockTTL                time.Duration
	BenchmarkGuard         bool
	GlobalLock             bool
	ExternalBenchmarkGuard bool
	Resident               ResidentConfig
}

type routingConfig struct {
	APIHost              string
	PreviewDomain        string
	APIHostAliases       []string
	PreviewDomainAliases []string
}

var supportedRoutes = map[string]map[string]routingConfig{
	"staging": {
		"us-central1": {APIHost: "api-staging.superserve.ai", PreviewDomain: "staging-sandbox.superserve.ai"},
	},
	"production": {
		"us-central1": {APIHost: "api.superserve.ai", PreviewDomain: "sandbox.superserve.ai"},
		"us-east4":    {APIHost: "api.superserve.ai", PreviewDomain: "sandbox.superserve.ai", PreviewDomainAliases: []string{"use-sandbox.superserve.ai"}},
		"us-west2":    {APIHost: "api-usw.superserve.ai", PreviewDomain: "usw-sandbox.superserve.ai", APIHostAliases: []string{"usw-api.superserve.ai"}},
	},
}

func Load() (Config, error) {
	cfg := Config{
		Scenario:         envDefault("LOAD_TEST_SCENARIO", "create"),
		Environment:      envDefault("CANARY_ENVIRONMENT", "staging"),
		ProductionOptIn:  productionOptIn(),
		Region:           strings.TrimSpace(os.Getenv("CANARY_REGION")),
		Target:           strings.TrimSpace(os.Getenv("CANARY_TARGET")),
		APIBaseURL:       strings.TrimSpace(os.Getenv("API_BASE_URL")),
		PreviewDomain:    strings.TrimSpace(os.Getenv("PREVIEW_DOMAIN")),
		Template:         envDefault("LOAD_TEST_SANDBOX_TEMPLATE", "superserve/python-3.11"),
		RunID:            strings.TrimSpace(os.Getenv("LOAD_TEST_RUN_ID")),
		WorkerID:         envDefault("LOAD_TEST_WORKER_ID", "worker-0"),
		Operations:       envInt("LOAD_TEST_OPERATIONS", 100),
		Concurrency:      envInt("LOAD_TEST_CONCURRENCY", 10),
		RunTimeout:       envDuration("LOAD_TEST_RUN_TIMEOUT", 30*time.Minute),
		ResourceTTL:      envDuration("LOAD_TEST_RESOURCE_TTL", 2*time.Hour),
		PollInterval:     envDuration("POLL_INTERVAL", 3*time.Second),
		HTTPTimeout:      envDuration("HTTP_TIMEOUT", 30*time.Second),
		CommandTimeout:   envDuration("COMMAND_TIMEOUT", 45*time.Second),
		ActiveTimeout:    envDuration("LOAD_TEST_ACTIVE_TIMEOUT", 2*time.Minute),
		DeleteTimeout:    envDuration("DELETE_TIMEOUT", 45*time.Second),
		SlackWebhookURL:  strings.TrimSpace(os.Getenv("CANARY_SLACK_WEBHOOK_URL")),
		TimeZone:         envDefault("LOAD_TEST_TIME_ZONE", "America/Los_Angeles"),
		DailyWindowStart: strings.TrimSpace(os.Getenv("LOAD_TEST_DAILY_WINDOW_START")),
		DailyWindowEnd:   strings.TrimSpace(os.Getenv("LOAD_TEST_DAILY_WINDOW_END")),
		LockBackend:      envDefault("CANARY_LOCK_BACKEND", "none"),
		LockBucket:       strings.TrimSpace(os.Getenv("LOCK_BUCKET")),
		LockFile:         strings.TrimSpace(os.Getenv("CANARY_LOCK_FILE")),
		LockTTL:          envDuration("LOAD_TEST_LOCK_TTL", 45*time.Minute),
	}
	var err error
	if cfg.Scheduled, err = loadBool("LOAD_TEST_SCHEDULED", false); err != nil {
		return Config{}, err
	}
	for _, item := range []struct {
		name  string
		value *bool
	}{
		{"LOAD_TEST_BENCHMARK_GUARD", &cfg.BenchmarkGuard},
		{"LOAD_TEST_GLOBAL_LOCK", &cfg.GlobalLock},
		{"LOAD_TEST_EXTERNAL_BENCHMARK_GUARD", &cfg.ExternalBenchmarkGuard},
	} {
		if *item.value, err = loadBool(item.name, cfg.Scheduled); err != nil {
			return Config{}, err
		}
	}
	cfg.APIKey = loadTestAPIKey(cfg.Environment)
	if cfg.RunID == "" {
		cfg.RunID = fmt.Sprintf("loadtest-%s", strings.ReplaceAll(uuid.NewString(), "-", ""))
	} else {
		cfg.RunID = safeRunID(cfg.RunID)
	}
	for name, value := range map[string]string{
		"CANARY_ENVIRONMENT": cfg.Environment,
		"CANARY_REGION":      cfg.Region,
		"CANARY_TARGET":      cfg.Target,
		"API_BASE_URL":       cfg.APIBaseURL,
		"PREVIEW_DOMAIN":     cfg.PreviewDomain,
		"CANARY_API_KEY":     cfg.APIKey,
	} {
		if value == "" {
			return Config{}, fmt.Errorf("%s is required", name)
		}
	}
	if cfg.Environment == "production" && !cfg.ProductionOptIn {
		return Config{}, errors.New("production load tests require LOAD_TEST_PRODUCTION_OPT_IN=true")
	}
	if cfg.Scenario != "create" && cfg.Scenario != "lifecycle" && cfg.Scenario != "resident" {
		return Config{}, errors.New("LOAD_TEST_SCENARIO must be create, lifecycle, or resident")
	}
	if cfg.Operations <= 0 || cfg.Operations > MaxOperations {
		return Config{}, fmt.Errorf("LOAD_TEST_OPERATIONS must be between 1 and %d", MaxOperations)
	}
	if cfg.Concurrency <= 0 || cfg.Concurrency > cfg.Operations {
		return Config{}, errors.New("LOAD_TEST_CONCURRENCY must be positive and no greater than LOAD_TEST_OPERATIONS")
	}
	if err := validateRouting(cfg.Environment, cfg.Region, cfg.Target, cfg.APIBaseURL, cfg.PreviewDomain); err != nil {
		return Config{}, err
	}
	if cfg.RunTimeout <= 0 || cfg.RunTimeout > maxRunTimeout ||
		cfg.ResourceTTL <= 0 || cfg.ResourceTTL > maxResourceTTL ||
		cfg.PollInterval <= 0 || cfg.PollInterval > maxPollInterval ||
		cfg.HTTPTimeout <= 0 || cfg.HTTPTimeout > maxHTTPTimeout ||
		cfg.CommandTimeout < time.Second || cfg.CommandTimeout > maxCommandTimeout ||
		cfg.ActiveTimeout <= 0 || cfg.ActiveTimeout > maxActiveTimeout ||
		cfg.DeleteTimeout <= 0 || cfg.DeleteTimeout > maxDeleteTimeout {
		return Config{}, errors.New("load runner durations must be positive and within their safety bounds")
	}
	if err := validateExecutionConfig(cfg); err != nil {
		return Config{}, err
	}
	if cfg.Scenario == "resident" {
		cfg.Resident, err = loadResidentConfig(cfg)
		if err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func validateExecutionConfig(cfg Config) error {
	switch cfg.LockBackend {
	case "none", "file", "gcs":
	default:
		return errors.New("CANARY_LOCK_BACKEND must be none, file, or gcs")
	}
	if cfg.LockBackend == "gcs" && cfg.LockBucket == "" {
		return errors.New("LOCK_BUCKET is required for GCS locking")
	}
	if cfg.LockBackend == "file" && cfg.LockFile == "" {
		return errors.New("CANARY_LOCK_FILE is required for file locking")
	}
	if (cfg.BenchmarkGuard || cfg.GlobalLock) && cfg.LockBackend != "gcs" {
		return errors.New("benchmark and global coordination require GCS locking")
	}
	if cfg.LockBackend != "none" && (cfg.LockTTL < cfg.RunTimeout+cfg.DeleteTimeout+time.Minute || cfg.LockTTL > 2*time.Hour) {
		return errors.New("LOAD_TEST_LOCK_TTL must cover runtime, cleanup and 1m reporting, and be at most 2h")
	}
	if cfg.ResourceTTL < cfg.RunTimeout+cfg.DeleteTimeout+time.Minute {
		return errors.New("LOAD_TEST_RESOURCE_TTL must cover runtime, cleanup and 1m reporting")
	}
	if cfg.SlackWebhookURL != "" {
		u, err := url.Parse(cfg.SlackWebhookURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return errors.New("CANARY_SLACK_WEBHOOK_URL must be an HTTPS URL without user information or a fragment")
		}
	}
	if cfg.Scheduled {
		if cfg.Scenario != "lifecycle" || cfg.LockBackend != "gcs" || !cfg.BenchmarkGuard || !cfg.GlobalLock || !cfg.ExternalBenchmarkGuard || cfg.SlackWebhookURL == "" {
			return errors.New("scheduled load tests require lifecycle mode, GCS coordination, all benchmark guards, and the summary webhook")
		}
		if _, err := time.LoadLocation(cfg.TimeZone); err != nil {
			return errors.New("LOAD_TEST_TIME_ZONE is not a recognized time zone")
		}
		start, startErr := time.Parse("15:04", cfg.DailyWindowStart)
		end, endErr := time.Parse("15:04", cfg.DailyWindowEnd)
		if startErr != nil || endErr != nil || len(cfg.DailyWindowStart) != 5 || len(cfg.DailyWindowEnd) != 5 || !end.After(start) || end.Sub(start) < cfg.RunTimeout+cfg.DeleteTimeout+time.Minute {
			return errors.New("daily load window must be same-day HH:MM values covering runtime, cleanup and 1m reporting")
		}
	}
	return nil
}

func loadBool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return parsed, nil
}

func validateRouting(environment, region, target, apiBaseURL, previewDomain string) error {
	if !validRoutingToken(environment) || !validRoutingToken(region) {
		return fmt.Errorf("invalid routing environment %q or region %q", environment, region)
	}
	environmentRoutes, ok := supportedRoutes[environment]
	if !ok {
		return fmt.Errorf("CANARY_ENVIRONMENT %q is unsupported", environment)
	}
	route, ok := environmentRoutes[region]
	if !ok {
		return fmt.Errorf("CANARY_REGION %q is unsupported for CANARY_ENVIRONMENT %q", region, environment)
	}
	expectedTarget := environment + "-" + region
	if target != expectedTarget {
		return fmt.Errorf("CANARY_TARGET %q does not match environment/region %q", target, expectedTarget)
	}
	parsed, err := url.Parse(apiBaseURL)
	if err != nil || parsed.Scheme != "https" || !routingHostAllowed(parsed.Hostname(), route.APIHost, route.APIHostAliases) || parsed.Host != parsed.Hostname() || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || strings.HasSuffix(apiBaseURL, "?") || strings.HasSuffix(apiBaseURL, "#") {
		return fmt.Errorf("API_BASE_URL does not match target %q", target)
	}
	if !routingHostAllowed(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(previewDomain), ".")), route.PreviewDomain, route.PreviewDomainAliases) {
		return fmt.Errorf("PREVIEW_DOMAIN does not match target %q", target)
	}
	return nil
}

func routingHostAllowed(value, canonical string, aliases []string) bool {
	if value == canonical {
		return true
	}
	for _, alias := range aliases {
		if value == alias {
			return true
		}
	}
	return false
}

func productionOptIn() bool {
	primary := strings.TrimSpace(os.Getenv("LOAD_TEST_PRODUCTION_OPT_IN"))
	legacy := strings.TrimSpace(os.Getenv("LOAD_TEST_ALLOW_PRODUCTION"))
	if primary != "" && legacy != "" && !strings.EqualFold(primary, legacy) {
		return false
	}
	value := primary
	if value == "" {
		value = legacy
	}
	return strings.EqualFold(value, "true")
}

func loadTestAPIKey(environment string) string {
	if key := strings.TrimSpace(os.Getenv("CANARY_API_KEY")); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv("CANARY_API_KEY_" + strings.ToUpper(strings.TrimSpace(environment))))
}

func validRoutingToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range strings.ToLower(value) {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return !strings.HasPrefix(value, "-") && !strings.HasSuffix(value, "-")
}

func safeRunID(raw string) string {
	raw = strings.TrimSpace(raw)
	id := normalizeIdentifier(raw, maxRunIDLength, runIDHashLength)
	if id == "" {
		return fmt.Sprintf("loadtest-%s", strings.ReplaceAll(uuid.NewString(), "-", ""))
	}
	if id == raw {
		return id
	}
	return id
}

func safeNamePart(raw string, maxLen int) string {
	return normalizeIdentifier(raw, maxLen, 8)
}

func normalizeIdentifier(raw string, maxLen, hashLen int) string {
	raw = strings.TrimSpace(raw)
	normalizedRaw := strings.ToLower(raw)
	var normalized strings.Builder
	separator := false
	for _, r := range normalizedRaw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if separator && normalized.Len() > 0 {
				normalized.WriteByte('-')
			}
			normalized.WriteRune(r)
			separator = false
		case normalized.Len() > 0:
			separator = true
		}
	}
	id := normalized.String()
	if id == "" {
		return ""
	}
	if len(id) <= maxLen && id == raw {
		return id
	}
	if hashLen <= 0 {
		hashLen = 8
	}
	if hashLen > maxLen-1 {
		hashLen = maxLen - 1
	}
	digest := sha256.Sum256([]byte(raw))
	suffix := hex.EncodeToString(digest[:])[:hashLen]
	maxPrefixLength := maxLen - len(suffix) - 1
	if maxPrefixLength < 1 {
		return suffix[:maxLen]
	}
	if len(id) > maxPrefixLength {
		id = id[:maxPrefixLength]
	}
	return id + "-" + suffix
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return -1
	}
	return parsed
}
