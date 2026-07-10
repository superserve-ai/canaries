package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/janitor"
	"github.com/superserve-ai/canaries/internal/lifecycle"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
)

func Run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("api-canary", flag.ContinueOnError)
	mode := fs.String("mode", envDefault("CANARY_MODE", "lifecycle"), "lifecycle or janitor")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*mode)
	if err != nil {
		return err
	}

	mp, shutdownMetrics, err := metrics.NewProvider(ctx, cfg.Metrics)
	if err != nil {
		return fmt.Errorf("init metrics: %w", err)
	}
	defer func() {
		if err := shutdownMetrics(context.Background()); err != nil {
			log.Error().Err(err).Msg("metrics shutdown failed")
		}
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

func newLocker(ctx context.Context, cfg config.Config) (lock.Locker, func(), error) {
	if cfg.Mode != config.ModeLifecycle {
		return lock.NoopLocker{}, func() {}, nil
	}
	if cfg.LockBucket == "" {
		return nil, nil, errors.New("LOCK_BUCKET is required for lifecycle mode")
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("create storage client: %w", err)
	}
	locker := lock.NewGCSLocker(client, cfg.LockBucket)
	return locker, func() { _ = client.Close() }, nil
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
