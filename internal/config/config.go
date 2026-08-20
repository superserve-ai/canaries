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

type Runtime string

type MetricsExporter string

type LockBackend string

const (
	ModeLifecycle   Mode = "lifecycle"
	ModeJanitor     Mode = "janitor"
	ModeUILifecycle Mode = "ui-lifecycle"

	RuntimeLocal    Runtime = "local"
	RuntimeCloudRun Runtime = "cloud-run"

	MetricsExporterNone   MetricsExporter = "none"
	MetricsExporterStdout MetricsExporter = "stdout"
	MetricsExporterOTLP   MetricsExporter = "otlp"

	LockBackendNone LockBackend = "none"
	LockBackendFile LockBackend = "file"
	LockBackendGCS  LockBackend = "gcs"
)

type Config struct {
	Mode                            Mode
	Runtime                         Runtime
	MetricsExporter                 MetricsExporter
	LockBackend                     LockBackend
	Target                          string
	Environment                     string
	Region                          string
	ProjectID                       string
	APIBaseURL                      string
	PreviewDomain                   string
	APIKey                          string
	SandboxTemplate                 string
	RunTimeout                      time.Duration
	ResourceTTL                     time.Duration
	PollInterval                    time.Duration
	LockTTL                         time.Duration
	LockBucket                      string
	LockFile                        string
	OTELExporterOTLPMetricsEndpoint string
	OTELExporterOTLPEndpoint        string
	HTTPTimeout                     time.Duration
	Metrics                         metricsConfig
	ManualStaging                   bool
	PreviewPort                     int
	RetainFailedSandbox             bool
	RetainFailedSandboxTTL          time.Duration
	CommandTimeout                  time.Duration
	DeleteTimeout                   time.Duration
	PreviewTimeout                  time.Duration
	JanitorThreshold                time.Duration
}

type metricsConfig struct {
	ServiceName string
	Environment string
}

func Load(rawMode string) (Config, error) {
	loadDotEnv()

	mode := Mode(strings.TrimSpace(rawMode))
	if mode != ModeLifecycle && mode != ModeJanitor && mode != ModeUILifecycle {
		return Config{}, fmt.Errorf("invalid mode %q", rawMode)
	}

	runtime := Runtime(strings.TrimSpace(envDefault("CANARY_RUNTIME", string(RuntimeLocal))))
	if runtime != RuntimeLocal && runtime != RuntimeCloudRun {
		return Config{}, fmt.Errorf("invalid CANARY_RUNTIME %q (must be local or cloud-run)", runtime)
	}

	metricsExporter := MetricsExporter(strings.TrimSpace(os.Getenv("CANARY_METRICS_EXPORTER")))
	if metricsExporter == "" {
		switch runtime {
		case RuntimeCloudRun:
			metricsExporter = MetricsExporterOTLP
		default:
			metricsExporter = MetricsExporterNone
		}
	}
	if !validMetricsExporter(metricsExporter) {
		return Config{}, fmt.Errorf("invalid CANARY_METRICS_EXPORTER %q (must be none, stdout, or otlp)", metricsExporter)
	}

	lockBackend := LockBackend(strings.TrimSpace(os.Getenv("CANARY_LOCK_BACKEND")))
	if lockBackend == "" {
		switch {
		case mode == ModeUILifecycle:
			lockBackend = LockBackendNone
		case runtime == RuntimeCloudRun:
			lockBackend = LockBackendGCS
		default:
			lockBackend = LockBackendFile
		}
	}
	if !validLockBackend(lockBackend) {
		return Config{}, fmt.Errorf("invalid CANARY_LOCK_BACKEND %q (must be none, file, or gcs)", lockBackend)
	}

	retainFailedSandbox, err := getenvBoolStrict("CANARY_RETAIN_FAILED_SANDBOX", false)
	if err != nil {
		return Config{}, err
	}
	retainFailedSandboxTTL, err := getenvDurationStrict("CANARY_RETAIN_FAILED_SANDBOX_TTL", 2*time.Hour)
	if err != nil {
		return Config{}, err
	}
	if retainFailedSandboxTTL <= 0 {
		return Config{}, errors.New("CANARY_RETAIN_FAILED_SANDBOX_TTL must be positive")
	}
	if retainFailedSandboxTTL > 24*time.Hour {
		return Config{}, errors.New("CANARY_RETAIN_FAILED_SANDBOX_TTL must be 24h or less")
	}

	target := os.Getenv("CANARY_TARGET")
	environment := os.Getenv("CANARY_ENVIRONMENT")
	region := os.Getenv("CANARY_REGION")
	projectID := os.Getenv("GCP_PROJECT_ID")
	if mode == ModeUILifecycle {
		target = envDefault("CANARY_TARGET", "staging-us-central1")
		environment = envDefault("CANARY_ENVIRONMENT", "staging")
		region = envDefault("CANARY_REGION", "us-central1")
		projectID = envDefault("GCP_PROJECT_ID", "superserve")
	}

	cfg := Config{
		Mode:                            mode,
		Runtime:                         runtime,
		MetricsExporter:                 metricsExporter,
		LockBackend:                     lockBackend,
		Target:                          target,
		Environment:                     environment,
		Region:                          region,
		ProjectID:                       projectID,
		APIBaseURL:                      os.Getenv("API_BASE_URL"),
		PreviewDomain:                   os.Getenv("PREVIEW_DOMAIN"),
		APIKey:                          os.Getenv("CANARY_API_KEY"),
		SandboxTemplate:                 envDefault("CANARY_SANDBOX_TEMPLATE", "superserve/python-3.11"),
		LockBucket:                      os.Getenv("LOCK_BUCKET"),
		LockFile:                        os.Getenv("CANARY_LOCK_FILE"),
		OTELExporterOTLPMetricsEndpoint: strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")),
		OTELExporterOTLPEndpoint:        strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		ManualStaging:                   getenvBool("MANUAL_STAGING_OPT_IN", false),
		PreviewPort:                     getenvInt("PREVIEW_PORT", 18080),
		RetainFailedSandbox:             retainFailedSandbox,
		RetainFailedSandboxTTL:          retainFailedSandboxTTL,
		RunTimeout:                      getenvDuration("RUN_TIMEOUT", 4*time.Minute),
		ResourceTTL:                     getenvDuration("RESOURCE_TTL", 1*time.Hour),
		PollInterval:                    getenvDuration("POLL_INTERVAL", 3*time.Second),
		LockTTL:                         getenvDuration("LOCK_TTL", 10*time.Minute),
		HTTPTimeout:                     getenvDuration("HTTP_TIMEOUT", 30*time.Second),
		CommandTimeout:                  getenvDuration("COMMAND_TIMEOUT", 45*time.Second),
		DeleteTimeout:                   getenvDuration("DELETE_TIMEOUT", 45*time.Second),
		PreviewTimeout:                  getenvDuration("PREVIEW_TIMEOUT", 20*time.Second),
		JanitorThreshold:                getenvDuration("JANITOR_STALE_THRESHOLD", 1*time.Hour),
		Metrics: metricsConfig{
			ServiceName: envDefault("OTEL_SERVICE_NAME", "superserve-api-canary"),
			Environment: envDefault("OTEL_ENVIRONMENT", environment),
		},
	}

	var requiredItems []struct {
		value string
		name  string
	}
	if mode == ModeUILifecycle {
		requiredItems = []struct {
			value string
			name  string
		}{
			{cfg.Target, "CANARY_TARGET"},
			{cfg.Environment, "CANARY_ENVIRONMENT"},
			{cfg.Region, "CANARY_REGION"},
			{cfg.ProjectID, "GCP_PROJECT_ID"},
		}
	} else {
		requiredItems = []struct {
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
		}
	}

	var missing []string
	for _, item := range requiredItems {
		if strings.TrimSpace(item.value) == "" {
			missing = append(missing, item.name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}

	if err := validateTargetTuple(cfg.Target, cfg.Environment, cfg.Region); err != nil {
		return Config{}, err
	}

	if cfg.LockBackend == LockBackendFile && cfg.LockFile == "" {
		cfg.LockFile = defaultLockFilePath(cfg.Target)
	}
	if cfg.LockBackend == LockBackendGCS && cfg.LockBucket == "" {
		return Config{}, errors.New("LOCK_BUCKET is required when CANARY_LOCK_BACKEND=gcs")
	}
	if cfg.OTELExporterOTLPMetricsEndpoint == "" && cfg.OTELExporterOTLPEndpoint != "" {
		cfg.OTELExporterOTLPMetricsEndpoint = cfg.OTELExporterOTLPEndpoint
	}

	switch cfg.Runtime {
	case RuntimeCloudRun:
		if cfg.MetricsExporter != MetricsExporterOTLP {
			return Config{}, fmt.Errorf("CANARY_RUNTIME=cloud-run requires CANARY_METRICS_EXPORTER=otlp")
		}
		if cfg.LockBackend != LockBackendGCS && cfg.Mode != ModeUILifecycle {
			return Config{}, fmt.Errorf("CANARY_RUNTIME=cloud-run requires CANARY_LOCK_BACKEND=gcs")
		}
		if cfg.OTELExporterOTLPMetricsEndpoint == "" {
			return Config{}, fmt.Errorf("CANARY_RUNTIME=cloud-run requires OTEL_EXPORTER_OTLP_METRICS_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT")
		}
		if cfg.LockBucket == "" && cfg.Mode != ModeUILifecycle {
			return Config{}, fmt.Errorf("CANARY_RUNTIME=cloud-run requires LOCK_BUCKET")
		}
	case RuntimeLocal:
	}

	if cfg.Environment == "staging" && !cfg.ManualStaging && os.Getenv("ALLOW_STAGING_MUTATION") == "true" {
		return Config{}, errors.New("manual staging runs require MANUAL_STAGING_OPT_IN=true")
	}

	return cfg, nil
}

func validMetricsExporter(v MetricsExporter) bool {
	switch v {
	case MetricsExporterNone, MetricsExporterStdout, MetricsExporterOTLP:
		return true
	default:
		return false
	}
}

func validLockBackend(v LockBackend) bool {
	switch v {
	case LockBackendNone, LockBackendFile, LockBackendGCS:
		return true
	default:
		return false
	}
}

func defaultLockFilePath(target string) string {
	return "/tmp/superserve-canary-" + sanitizeTarget(target) + ".lock"
}

func sanitizeTarget(target string) string {
	target = strings.TrimSpace(strings.ToLower(target))
	if target == "" {
		return "target"
	}
	var b strings.Builder
	b.Grow(len(target))
	lastDash := false
	for _, r := range target {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "target"
	}
	return out
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

func getenvBoolStrict(key string, fallback bool) (bool, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("invalid %s %q", key, value)
	}
	return parsed, nil
}

func getenvDurationStrict(key string, fallback time.Duration) (time.Duration, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", key, value)
	}
	return parsed, nil
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

func loadDotEnv() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eqIdx := strings.Index(line, "=")
		if eqIdx == -1 {
			continue
		}
		key := strings.TrimSpace(line[:eqIdx])
		val := strings.TrimSpace(line[eqIdx+1:])
		if (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) ||
			(strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) {
			if len(val) >= 2 {
				val = val[1 : len(val)-1]
			}
		}
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

// validateTargetTuple checks that target is a two-part hyphen-separated string
// whose first part equals environment and second part equals region.
// This is only enforced for ModeUILifecycle to catch misconfigured deployments early.
func validateTargetTuple(target, environment, region string) error {
	parts := strings.SplitN(target, "-", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("CANARY_TARGET %q must be in <environment>-<region> format (e.g. staging-us-central1)", target)
	}
	if parts[0] != environment {
		return fmt.Errorf("CANARY_TARGET %q environment part %q does not match CANARY_ENVIRONMENT %q", target, parts[0], environment)
	}
	if parts[1] != region {
		return fmt.Errorf("CANARY_TARGET %q region part %q does not match CANARY_REGION %q", target, parts[1], region)
	}
	return nil
}
