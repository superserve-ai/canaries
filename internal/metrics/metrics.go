package metrics

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otlpmetrichttp "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

type Config struct {
	ServiceName string
	Environment string
}

type Provider struct {
	meter           metric.Meter
	runTotal        metric.Int64Counter
	stepTotal       metric.Int64Counter
	cleanupTotal    metric.Int64Counter
	overlapSkipped  metric.Int64Counter
	orphanResources metric.Int64UpDownCounter
	runDuration     metric.Float64Histogram
	stepDuration    metric.Float64Histogram
	lastSuccess     metric.Float64Gauge
}

func NewProvider(ctx context.Context, cfg any) (Provider, func(context.Context) error, error) {
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return Provider{}, nil, err
	}
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

	return Provider{
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

func (p Provider) attrs(environment, region, target, scenario, step, result string) []attribute.KeyValue {
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

func (p Provider) RecordRun(ctx context.Context, environment, region, target, scenario, result string, duration time.Duration) {
	if p.runTotal == nil || p.runDuration == nil {
		return
	}
	attrs := p.attrs(environment, region, target, scenario, "", result)
	p.runTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	p.runDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	if result == "success" && p.lastSuccess != nil {
		p.lastSuccess.Record(ctx, float64(time.Now().Unix()), metric.WithAttributes(attrs...))
	}
}

func (p Provider) RecordStep(ctx context.Context, environment, region, target, scenario, step, result string, duration time.Duration) {
	if p.stepTotal == nil || p.stepDuration == nil {
		return
	}
	attrs := p.attrs(environment, region, target, scenario, step, result)
	p.stepTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	p.stepDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
}

func (p Provider) RecordCleanup(ctx context.Context, environment, region, target, result string) {
	if p.cleanupTotal == nil {
		return
	}
	p.cleanupTotal.Add(ctx, 1, metric.WithAttributes(p.attrs(environment, region, target, "cleanup", "", result)...))
}

func (p Provider) RecordOverlapSkip(ctx context.Context, environment, region, target string) {
	if p.overlapSkipped == nil {
		return
	}
	p.overlapSkipped.Add(ctx, 1, metric.WithAttributes(p.attrs(environment, region, target, "lifecycle", "", "skipped")...))
}

func (p Provider) RecordOrphans(ctx context.Context, environment, region, target string, count int64) {
	if p.orphanResources == nil {
		return
	}
	p.orphanResources.Add(ctx, count, metric.WithAttributes(p.attrs(environment, region, target, "janitor", "", "observed")...))
}
