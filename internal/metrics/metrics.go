package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"

	"github.com/superserve-ai/canaries/internal/config"
)

type Provider interface {
	RecordRun(context.Context, string, string, string, string, string, time.Duration)
	RecordStep(context.Context, string, string, string, string, string, string, time.Duration)
	RecordCleanup(context.Context, string, string, string, string)
	RecordOverlapSkip(context.Context, string, string, string)
	RecordExecutionDelta(context.Context, string, string, string, string, int64)
	RecordOrphans(context.Context, string, string, string, int64, time.Duration)
	RecordRetainedSandbox(context.Context, string, string, string, string)
	RecordJanitorResources(context.Context, string, string, string, int64, int64, int64)
}

type NoopProvider struct{}

func (NoopProvider) RecordRun(context.Context, string, string, string, string, string, time.Duration) {
}
func (NoopProvider) RecordStep(context.Context, string, string, string, string, string, string, time.Duration) {
}
func (NoopProvider) RecordCleanup(context.Context, string, string, string, string) {}
func (NoopProvider) RecordOverlapSkip(context.Context, string, string, string)     {}
func (NoopProvider) RecordExecutionDelta(context.Context, string, string, string, string, int64) {
}
func (NoopProvider) RecordOrphans(context.Context, string, string, string, int64, time.Duration) {}
func (NoopProvider) RecordRetainedSandbox(context.Context, string, string, string, string) {
}
func (NoopProvider) RecordJanitorResources(context.Context, string, string, string, int64, int64, int64) {
}

type recorder struct {
	meter                    metric.Meter
	runTotal                 metric.Int64Counter
	stepTotal                metric.Int64Counter
	cleanupTotal             metric.Int64Counter
	overlapSkipped           metric.Int64Counter
	runningExecutions        metric.Int64UpDownCounter
	orphanResources          metric.Float64Gauge
	oldestOrphanAge          metric.Float64Gauge
	retainedSandbox          metric.Int64Counter
	janitorResourcesExamined metric.Int64Counter
	janitorResourcesDeleted  metric.Int64Counter
	janitorDeleteFailures    metric.Int64Counter
	runDuration              metric.Float64Histogram
	stepDuration             metric.Float64Histogram
	lastCompleted            metric.Float64Gauge
	lastSuccess              metric.Float64Gauge
	lastSuccessUnix          atomic.Int64
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
		httpClient, err := otlpHTTPClient(ctx, cfg.OTELExporterOTLPMetricsEndpoint)
		if err != nil {
			return nil, nil, err
		}
		exporter, err := otlpmetrichttp.New(
			ctx,
			otlpmetrichttp.WithEndpointURL(cfg.OTELExporterOTLPMetricsEndpoint),
			otlpmetrichttp.WithURLPath("/v1/metrics"),
			otlpmetrichttp.WithHTTPClient(httpClient),
		)
		if err != nil {
			return nil, nil, err
		}
		return newSDKProvider(ctx, exporter)
	default:
		return nil, nil, fmt.Errorf("unsupported metrics exporter %q", cfg.MetricsExporter)
	}
}

func otlpHTTPClient(ctx context.Context, endpoint string) (*http.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid OTLP endpoint %q: %w", endpoint, err)
	}
	if u.Host != "telemetry.googleapis.com" {
		return http.DefaultClient, nil
	}
	client, _, err := transport.NewHTTPClient(ctx, option.WithScopes("https://www.googleapis.com/auth/cloud-platform"))
	if err != nil {
		return nil, err
	}
	return client, nil
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
	runningExecutions, _ := meter.Int64UpDownCounter("superserve_canary_running_executions")
	orphanResources, _ := meter.Float64Gauge("superserve_canary_orphan_resources")
	oldestOrphanAge, _ := meter.Float64Gauge("superserve_canary_oldest_orphan_age_seconds")
	retainedSandbox, _ := meter.Int64Counter("superserve_canary_retained_sandbox_total")
	janitorResourcesExamined, _ := meter.Int64Counter("superserve_canary_janitor_resources_examined_total")
	janitorResourcesDeleted, _ := meter.Int64Counter("superserve_canary_janitor_resources_deleted_total")
	janitorDeleteFailures, _ := meter.Int64Counter("superserve_canary_janitor_delete_failures_total")
	runDuration, _ := meter.Float64Histogram("superserve_canary_run_duration_seconds")
	stepDuration, _ := meter.Float64Histogram("superserve_canary_step_duration_seconds")
	lastCompleted, _ := meter.Float64Gauge("superserve_canary_last_completed_timestamp_seconds")
	lastSuccess, _ := meter.Float64Gauge("superserve_canary_last_success_timestamp_seconds")

	return &recorder{
		meter:                    meter,
		runTotal:                 runTotal,
		stepTotal:                stepTotal,
		cleanupTotal:             cleanupTotal,
		overlapSkipped:           overlapSkipped,
		runningExecutions:        runningExecutions,
		orphanResources:          orphanResources,
		oldestOrphanAge:          oldestOrphanAge,
		retainedSandbox:          retainedSandbox,
		janitorResourcesExamined: janitorResourcesExamined,
		janitorResourcesDeleted:  janitorResourcesDeleted,
		janitorDeleteFailures:    janitorDeleteFailures,
		runDuration:              runDuration,
		stepDuration:             stepDuration,
		lastCompleted:            lastCompleted,
		lastSuccess:              lastSuccess,
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
	if p.lastCompleted != nil {
		ts := float64(time.Now().Unix())
		p.lastCompleted.Record(ctx, ts, metric.WithAttributes(attrs...))
	}
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

func (p *recorder) RecordExecutionDelta(ctx context.Context, environment, region, target, scenario string, delta int64) {
	if p == nil || p.runningExecutions == nil {
		return
	}
	p.runningExecutions.Add(ctx, delta, metric.WithAttributes(p.attrs(environment, region, target, scenario, "", "running")...))
}

func (p *recorder) RecordOrphans(ctx context.Context, environment, region, target string, count int64, oldestAge time.Duration) {
	if p == nil || p.orphanResources == nil {
		return
	}
	attrs := p.attrs(environment, region, target, "janitor", "", "observed")
	p.orphanResources.Record(ctx, float64(count), metric.WithAttributes(attrs...))
	if p.oldestOrphanAge != nil {
		p.oldestOrphanAge.Record(ctx, oldestAge.Seconds(), metric.WithAttributes(attrs...))
	}
}

func (p *recorder) RecordRetainedSandbox(ctx context.Context, environment, region, target, failedStep string) {
	if p == nil || p.retainedSandbox == nil {
		return
	}
	attrs := p.attrs(environment, region, target, "lifecycle", failedStep, "retained")
	p.retainedSandbox.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func (p *recorder) RecordJanitorResources(ctx context.Context, environment, region, target string, examined, deleted, deleteFailures int64) {
	if p == nil {
		return
	}
	base := p.attrs(environment, region, target, "janitor", "", "observed")
	if p.janitorResourcesExamined != nil {
		p.janitorResourcesExamined.Add(ctx, examined, metric.WithAttributes(base...))
	}
	if p.janitorResourcesDeleted != nil {
		p.janitorResourcesDeleted.Add(ctx, deleted, metric.WithAttributes(base...))
	}
	if p.janitorDeleteFailures != nil {
		p.janitorDeleteFailures.Add(ctx, deleteFailures, metric.WithAttributes(base...))
	}
}
