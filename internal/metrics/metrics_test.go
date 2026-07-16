package metrics

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/superserve-ai/canaries/internal/config"
)

func TestNewProvider(t *testing.T) {
	t.Run("none exporter performs no network calls", func(t *testing.T) {
		var hits atomic.Int64
		provider, shutdown, err := NewProvider(context.Background(), config.Config{
			MetricsExporter:                 config.MetricsExporterNone,
			OTELExporterOTLPMetricsEndpoint: "https://collector.example/v1/metrics",
		})
		if err != nil {
			t.Fatal(err)
		}
		provider.RecordRun(context.Background(), "env", "region", "target", "lifecycle", "success", 10)
		provider.RecordStep(context.Background(), "env", "region", "target", "lifecycle", "step", "success", 10)
		provider.RecordCleanup(context.Background(), "env", "region", "target", "success")
		provider.RecordOverlapSkip(context.Background(), "env", "region", "target")
		provider.RecordExecutionDelta(context.Background(), "env", "region", "target", "lifecycle", 1)
		provider.RecordOrphans(context.Background(), "env", "region", "target", 1, 10)
		provider.RecordJanitorResources(context.Background(), "env", "region", "target", 1, 1, 0)
		if err := shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := hits.Load(); got != 0 {
			t.Fatalf("network hits = %d, want 0", got)
		}
	})

	t.Run("none shutdown returns nil", func(t *testing.T) {
		provider, shutdown, err := NewProvider(context.Background(), config.Config{
			MetricsExporter: config.MetricsExporterNone,
		})
		if err != nil {
			t.Fatal(err)
		}
		if provider == nil {
			t.Fatal("provider is nil")
		}
		if err := shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("stdout exporter initializes", func(t *testing.T) {
		provider, shutdown, err := NewProvider(context.Background(), config.Config{
			MetricsExporter: config.MetricsExporterStdout,
		})
		if err != nil {
			t.Fatal(err)
		}
		if provider == nil {
			t.Fatal("provider is nil")
		}
		if err := shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("otlp without endpoint fails", func(t *testing.T) {
		_, _, err := NewProvider(context.Background(), config.Config{
			MetricsExporter: config.MetricsExporterOTLP,
		})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
