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
	"github.com/superserve-ai/canaries/internal/metrics"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadrunner.Load()
	if err != nil {
		fail(err)
	}
	log.Info().Str("run_id", cfg.RunID).Str("environment", cfg.Environment).Int("operations", cfg.Operations).Int("concurrency", cfg.Concurrency).Dur("run_timeout", cfg.RunTimeout).Msg("starting load test")

	metricsCfg := loadrunnerMetricsConfig()
	mp, shutdownMetrics, err := metrics.NewProvider(ctx, metricsCfg)
	if err != nil {
		fail(err)
	}
	defer func() {
		if shutdownErr := shutdownMetrics(context.Background()); shutdownErr != nil {
			log.Warn().Err(shutdownErr).Msg("metrics shutdown failed")
		}
	}()

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	client := canaryapi.NewClient(httpClient, cfg.APIBaseURL, cfg.APIKey, cfg.PreviewDomain)
	runner := loadrunner.Runner{Config: cfg, Ops: lifecycle.Operations{Client: client, Metrics: mp, Clock: time.Now, HTTP: httpClient}, Clock: time.Now}
	_, err = runner.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		fail(err)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func fail(err error) {
	log.Error().Err(err).Msg("load runner failed")
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
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
