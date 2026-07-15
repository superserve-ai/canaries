package app

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/janitor"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
)

func Run(ctx context.Context, args []string) (err error) {
	fs := flag.NewFlagSet("api-canary", flag.ContinueOnError)
	mode := fs.String("mode", envDefault("CANARY_MODE", "lifecycle"), "lifecycle or janitor")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*mode)
	if err != nil {
		return err
	}

	log.Info().
		Str("runtime", string(cfg.Runtime)).
		Str("metrics_exporter", string(cfg.MetricsExporter)).
		Str("lock_backend", string(cfg.LockBackend)).
		Msg("canary configuration")
	if cfg.LockBackend == config.LockBackendNone {
		log.Info().Msg("locking disabled for this run")
	}

	mp, shutdownMetrics, err := metrics.NewProvider(ctx, cfg)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer func() {
		shutdownErr := shutdownMetrics(context.Background())
		err = combineRunAndShutdownError(err, shutdownErr, cfg.Runtime)
	}()

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	apiClient := canaryapi.NewClient(httpClient, cfg.APIBaseURL, cfg.APIKey, cfg.PreviewDomain)

	switch cfg.Mode {
	case config.ModeLifecycle:
		locker, closeFn, err := newLocker(ctx, cfg)
		if err != nil {
			return err
		}
		defer closeFn()

		runner := lifecycle.Runner{
			Config:  cfg,
			Client:  apiClient,
			Locker:  locker,
			Metrics: mp,
			Clock:   time.Now,
		}
		return runner.Run(ctx)
	case config.ModeJanitor:
		runner := janitor.Runner{
			Config:  cfg,
			Client:  apiClient,
			Metrics: mp,
			Clock:   time.Now,
		}
		return runner.Run(ctx)
	default:
		return fmt.Errorf("unsupported mode %q", cfg.Mode)
	}
}

func newLocker(ctx context.Context, cfg config.Config) (lock.Lock, func(), error) {
	if cfg.Mode != config.ModeLifecycle {
		return lock.NoopLock{}, func() {}, nil
	}
	locker, closeFn, err := lock.New(ctx, lock.Config{
		Backend:  string(cfg.LockBackend),
		FilePath: cfg.LockFile,
		Bucket:   cfg.LockBucket,
	})
	if err != nil {
		return nil, nil, err
	}
	return locker, closeFn, nil
}

func combineRunAndShutdownError(runErr, shutdownErr error, runtime config.Runtime) error {
	if shutdownErr == nil {
		return runErr
	}
	if runErr != nil {
		log.Warn().Err(shutdownErr).Msg("metrics shutdown failed")
		return runErr
	}
	if runtime == config.RuntimeCloudRun {
		return shutdownErr
	}
	log.Warn().Err(shutdownErr).Msg("metrics shutdown failed")
	return nil
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
