package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/loadrunner"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
	"github.com/superserve-ai/canaries/internal/slack"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadrunner.Load()
	if err != nil {
		return fail(err)
	}
	log.Info().Str("run_id", cfg.RunID).Str("environment", cfg.Environment).Int("operations", cfg.Operations).Int("concurrency", cfg.Concurrency).Dur("run_timeout", cfg.RunTimeout).Msg("starting load test")

	err = runConfigured(ctx, cfg, runDependencies{})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fail(err)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type runDependencies struct {
	Now        func() time.Time
	HTTP       *http.Client
	NewMetrics func(context.Context, config.Config) (metrics.Provider, func(context.Context) error, error)
	NewLocker  func(context.Context, lock.Config) (lock.Lock, func(), error)
}

func runConfigured(ctx context.Context, cfg loadrunner.Config, deps runDependencies) (runErr error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if err := loadrunner.ValidateResidentAdmission(cfg, deps.Now()); err != nil {
		return err
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	if deps.NewMetrics == nil {
		deps.NewMetrics = metrics.NewProvider
	}
	if deps.NewLocker == nil {
		deps.NewLocker = lock.New
	}
	scenario := "loadtest-create"
	if cfg.Scenario == "lifecycle" {
		scenario = "loadtest-lifecycle"
	} else if cfg.Scenario == "resident" {
		scenario = "loadtest-resident"
	}
	summary := loadrunner.Summary{
		ConfiguredOperations: int64(cfg.Operations), ConfiguredConcurrency: int64(cfg.Concurrency), Unstarted: int64(cfg.Operations),
	}
	var provider metrics.Provider = metrics.NoopProvider{}
	shutdown := func(context.Context) error { return nil }
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdown(shutdownCtx); err != nil {
			runErr = errors.Join(runErr, errors.New("load test metrics export failed"))
		}
		report := summary.Report(cfg, runErr)
		log.Info().Str("run_id", cfg.RunID).Str("summary", report).Msg("load testing summary")
		if cfg.SlackWebhookURL != "" {
			if err := slack.Post(context.Background(), deps.HTTP, cfg.SlackWebhookURL, report); err != nil {
				// HTTP errors can contain the secret webhook URL. Keep them out
				// of logs, returned errors, and the public result summary.
				runErr = errors.Join(runErr, errors.New("load test summary delivery failed"))
			}
		}
		result := "success"
		if runErr != nil {
			result = "failure"
		}
		// Overall success cannot be exported through a provider whose shutdown
		// may still fail. The terminal log and Cloud Run execution metric cover
		// the full job outcome, including metrics and summary delivery.
		event := log.Info()
		if runErr != nil {
			event = log.Error().Err(runErr)
		}
		event.Str("run_id", cfg.RunID).Str("environment", cfg.Environment).Str("region", cfg.Region).
			Str("target", cfg.Target).Str("result", result).Int("configured_operations", cfg.Operations).
			Int("configured_concurrency", cfg.Concurrency).Int64("started", summary.Started).
			Int64("completed", summary.Completed).Int64("failed", summary.Failed).Int64("unstarted", summary.Unstarted).
			Int64("peak_in_flight", summary.PeakInFlight).Int64("cleanup_failures", summary.CleanupFailures).
			Dur("duration", summary.Duration).Msg("load test completed")
	}()

	mp, closeMetrics, err := deps.NewMetrics(ctx, loadrunnerMetricsConfig())
	if err != nil {
		return fmt.Errorf("initialize load test metrics: %w", err)
	}
	provider, shutdown = mp, closeMetrics
	locker, closeLocker, err := deps.NewLocker(ctx, lock.Config{Backend: cfg.LockBackend, Bucket: cfg.LockBucket, FilePath: cfg.LockFile})
	if err != nil {
		return fmt.Errorf("initialize load test coordination: %w", err)
	}
	defer closeLocker()

	guard, err := loadrunner.AcquireGuard(ctx, cfg, locker, deps.Now, deps.HTTP)
	if err != nil {
		if errors.Is(err, loadrunner.ErrGuardBlocked) {
			provider.RecordOverlapSkip(context.Background(), cfg.Environment, cfg.Region, cfg.Target, scenario)
		}
		return err
	}
	// This scope releases all leases while the lock client is still open and
	// before the final report is posted, including cancellation/failure paths.
	return func() (err error) {
		defer func() { err = errors.Join(err, guard.Release(context.Background())) }()
		provider.RecordExecutionDelta(ctx, cfg.Environment, cfg.Region, cfg.Target, scenario, 1)
		defer provider.RecordExecutionDelta(context.Background(), cfg.Environment, cfg.Region, cfg.Target, scenario, -1)
		workCtx := ctx
		if !guard.Deadline.IsZero() {
			var cancel context.CancelFunc
			workCtx, cancel = context.WithDeadline(ctx, guard.Deadline)
			defer cancel()
		}
		client := canaryapi.NewClient(deps.HTTP, cfg.APIBaseURL, cfg.APIKey, cfg.PreviewDomain)
		runner := loadrunner.Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: provider, Clock: deps.Now, HTTP: deps.HTTP}, Clock: deps.Now}
		summary, err = runner.Run(workCtx)
		return err
	}()
}

func fail(err error) int {
	log.Error().Err(err).Msg("load runner failed")
	fmt.Fprintln(os.Stderr, err)
	return 1
}

func loadrunnerMetricsConfig() config.Config {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}

	metricsExporter := config.MetricsExporter(strings.TrimSpace(os.Getenv("CANARY_METRICS_EXPORTER")))
	if metricsExporter == "" {
		if endpoint != "" {
			metricsExporter = config.MetricsExporterOTLP
		} else {
			metricsExporter = config.MetricsExporterNone
		}
	}

	return config.Config{
		MetricsExporter:                 metricsExporter,
		OTELExporterOTLPMetricsEndpoint: strings.TrimSpace(endpoint),
	}
}
