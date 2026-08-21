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
	Environment     string
	ProductionOptIn bool
	Region          string
	Target          string
	APIBaseURL      string
	PreviewDomain   string
	APIKey          string
	Template        string
	RunID           string
	WorkerID        string
	Operations      int
	Concurrency     int
	RunTimeout      time.Duration
	ResourceTTL     time.Duration
	PollInterval    time.Duration
	HTTPTimeout     time.Duration
	CommandTimeout  time.Duration
	ActiveTimeout   time.Duration
	DeleteTimeout   time.Duration
}

type routingConfig struct {
	APIHost       string
	PreviewDomain string
}

var supportedRoutes = map[string]map[string]routingConfig{
	"staging": {
		"us-central1": {APIHost: "api-staging.superserve.ai", PreviewDomain: "staging-sandbox.superserve.ai"},
	},
	"production": {
		"us-central1": {APIHost: "api.superserve.ai", PreviewDomain: "sandbox.superserve.ai"},
		"us-east4":    {APIHost: "api.superserve.ai", PreviewDomain: "use-sandbox.superserve.ai"},
		"us-west2":    {APIHost: "usw-api.superserve.ai", PreviewDomain: "usw-sandbox.superserve.ai"},
	},
}

func Load() (Config, error) {
	cfg := Config{
		Environment:     envDefault("CANARY_ENVIRONMENT", "staging"),
		ProductionOptIn: productionOptIn(),
		Region:          strings.TrimSpace(os.Getenv("CANARY_REGION")),
		Target:          strings.TrimSpace(os.Getenv("CANARY_TARGET")),
		APIBaseURL:      strings.TrimSpace(os.Getenv("API_BASE_URL")),
		PreviewDomain:   strings.TrimSpace(os.Getenv("PREVIEW_DOMAIN")),
		Template:        envDefault("LOAD_TEST_SANDBOX_TEMPLATE", "superserve/python-3.11"),
		RunID:           strings.TrimSpace(os.Getenv("LOAD_TEST_RUN_ID")),
		WorkerID:        envDefault("LOAD_TEST_WORKER_ID", "worker-0"),
		Operations:      envInt("LOAD_TEST_OPERATIONS", 100),
		Concurrency:     envInt("LOAD_TEST_CONCURRENCY", 10),
		RunTimeout:      envDuration("LOAD_TEST_RUN_TIMEOUT", 30*time.Minute),
		ResourceTTL:     envDuration("LOAD_TEST_RESOURCE_TTL", 2*time.Hour),
		PollInterval:    envDuration("POLL_INTERVAL", 3*time.Second),
		HTTPTimeout:     envDuration("HTTP_TIMEOUT", 30*time.Second),
		CommandTimeout:  envDuration("COMMAND_TIMEOUT", 45*time.Second),
		ActiveTimeout:   envDuration("LOAD_TEST_ACTIVE_TIMEOUT", 2*time.Minute),
		DeleteTimeout:   envDuration("DELETE_TIMEOUT", 45*time.Second),
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
	return cfg, nil
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
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != route.APIHost || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || strings.HasSuffix(apiBaseURL, "?") || strings.HasSuffix(apiBaseURL, "#") {
		return fmt.Errorf("API_BASE_URL %q does not match target %q", apiBaseURL, target)
	}
	if strings.ToLower(strings.TrimSuffix(strings.TrimSpace(previewDomain), ".")) != route.PreviewDomain {
		return fmt.Errorf("PREVIEW_DOMAIN %q does not match target %q", previewDomain, target)
	}
	return nil
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
