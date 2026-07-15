package metrics

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/superserve-ai/canaries/internal/config"
)

type Provider interface {
	RecordRun(context.Context, string, string, string, string, string, time.Duration)
	RecordStep(context.Context, string, string, string, string, string, string, time.Duration)
	RecordCleanup(context.Context, string, string, string, string)
	RecordOverlapSkip(context.Context, string, string, string)
	RecordOrphans(context.Context, string, string, string, int64)
}

type NoopProvider struct{}

func (NoopProvider) RecordRun(context.Context, string, string, string, string, string, time.Duration) {
}
func (NoopProvider) RecordStep(context.Context, string, string, string, string, string, string, time.Duration) {
}
func (NoopProvider) RecordCleanup(context.Context, string, string, string, string) {}
func (NoopProvider) RecordOverlapSkip(context.Context, string, string, string)     {}
func (NoopProvider) RecordOrphans(context.Context, string, string, string, int64)  {}

type recorder struct {
	meter           metric.Meter
	runTotal        metric.Int64Counter
	stepTotal       metric.Int64Counter
	cleanupTotal    metric.Int64Counter
	overlapSkipped  metric.Int64Counter
	orphanResources metric.Int64UpDownCounter
	runDuration     metric.Float64Histogram
	stepDuration    metric.Float64Histogram
	lastSuccess     metric.Float64Gauge
	lastSuccessUnix atomic.Int64
}

func NewProvider(ctx context.Context, cfg config.Config) (Provider, func(context.Context) error, error) {
	switch cfg.MetricsExporter {
	case config.MetricsExporterNone:
		return NoopProvider{}, func(context.Context) error { return nil }, nil
	case config.MetricsExporterStdout:
		exporter, err := stdoutmetric.New()
		if err != nil {
			return nil, nil, err
		}
		return newSDKProvider(ctx, exporter)
	case config.MetricsExporterOTLP:
		if cfg.OTELExporterOTLPMetricsEndpoint == "" {
			return nil, nil, errors.New("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT is required when CANARY_METRICS_EXPORTER=otlp")
		}
		exporter, err := otlpmetrichttp.New(
			ctx,
			otlpmetrichttp.WithEndpointURL(cfg.OTELExporterOTLPMetricsEndpoint),
			otlpmetrichttp.WithURLPath("/v1/metrics"),
		)
		if err != nil {
			return nil, nil, err
		}
		return newSDKProvider(ctx, exporter)
	default:
		return nil, nil, fmt.Errorf("unsupported metrics exporter %q", cfg.MetricsExporter)
	}
}

func newSDKProvider(_ context.Context, exporter sdkmetric.Exporter) (Provider, func(context.Context) error, error) {
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(15*time.Second))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	meter := mp.Meter("github.com/superserve-ai/canaries")

	runTotal, _ := meter.Int64Counter("superserve_canary_run_total")
	stepTotal, _ := meter.Int64Counter("superserve_canary_step_total")
	cleanupTotal, _ := meter.Int64Counter("superserve_canary_cleanup_total")
	overlapSkipped, _ := meter.Int64Counter("superserve_canary_overlap_skipped_total")
	orphanResources, _ := meter.Int64UpDownCounter("superserve_canary_orphan_resources")
	runDuration, _ := meter.Float64Histogram("superserve_canary_run_duration_seconds")
	stepDuration, _ := meter.Float64Histogram("superserve_canary_step_duration_seconds")
	lastSuccess, _ := meter.Float64Gauge("superserve_canary_last_success_timestamp_seconds")

	return &recorder{
		meter:           meter,
		runTotal:        runTotal,
		stepTotal:       stepTotal,
		cleanupTotal:    cleanupTotal,
		overlapSkipped:  overlapSkipped,
		orphanResources: orphanResources,
		runDuration:     runDuration,
		stepDuration:    stepDuration,
		lastSuccess:     lastSuccess,
	}, mp.Shutdown, nil
}

func (p *recorder) attrs(environment, region, target, scenario, step, result string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("environment", environment),
		attribute.String("region", region),
		attribute.String("target", target),
		attribute.String("scenario", scenario),
		attribute.String("result", result),
	}
	if step != "" {
		attrs = append(attrs, attribute.String("step", step))
	}
	return attrs
}

func (p *recorder) RecordRun(ctx context.Context, environment, region, target, scenario, result string, duration time.Duration) {
	if p == nil || p.runTotal == nil || p.runDuration == nil {
		return
	}
	attrs := p.attrs(environment, region, target, scenario, "", result)
	p.runTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	p.runDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	if result == "success" && p.lastSuccess != nil {
		ts := float64(time.Now().Unix())
		p.lastSuccess.Record(ctx, ts, metric.WithAttributes(attrs...))
		p.lastSuccessUnix.Store(int64(ts))
	}
}

func (p *recorder) RecordStep(ctx context.Context, environment, region, target, scenario, step, result string, duration time.Duration) {
	if p == nil || p.stepTotal == nil || p.stepDuration == nil {
		return
	}
	attrs := p.attrs(environment, region, target, scenario, step, result)
	p.stepTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	p.stepDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

func (p *recorder) RecordCleanup(ctx context.Context, environment, region, target, result string) {
	if p == nil || p.cleanupTotal == nil {
		return
	}
	p.cleanupTotal.Add(ctx, 1, metric.WithAttributes(p.attrs(environment, region, target, "cleanup", "", result)...))
}

func (p *recorder) RecordOverlapSkip(ctx context.Context, environment, region, target string) {
	if p == nil || p.overlapSkipped == nil {
		return
	}
	p.overlapSkipped.Add(ctx, 1, metric.WithAttributes(p.attrs(environment, region, target, "lifecycle", "", "skipped")...))
}

func (p *recorder) RecordOrphans(ctx context.Context, environment, region, target string, count int64) {
	if p == nil || p.orphanResources == nil {
		return
	}
	p.orphanResources.Add(ctx, count, metric.WithAttributes(p.attrs(environment, region, target, "janitor", "", "observed")...))
}
