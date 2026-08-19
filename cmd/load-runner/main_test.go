package main

import (
	"os"
	"testing"

	"github.com/superserve-ai/canaries/internal/config"
)

func TestLoadrunnerMetricsConfigDefaultsToOTLPWhenEndpointIsSet(t *testing.T) {
	t.Setenv("CANARY_METRICS_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example/v1/metrics")

	cfg := loadrunnerMetricsConfig()
	if cfg.MetricsExporter != config.MetricsExporterOTLP {
		t.Fatalf("MetricsExporter = %q, want %q", cfg.MetricsExporter, config.MetricsExporterOTLP)
	}
	if got := cfg.OTELExporterOTLPMetricsEndpoint; got != "https://collector.example/v1/metrics" {
		t.Fatalf("OTELExporterOTLPMetricsEndpoint = %q", got)
	}
}

func TestLoadrunnerMetricsConfigRespectsExplicitExporter(t *testing.T) {
	t.Setenv("CANARY_METRICS_EXPORTER", string(config.MetricsExporterStdout))
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example/v1/metrics")

	cfg := loadrunnerMetricsConfig()
	if cfg.MetricsExporter != config.MetricsExporterStdout {
		t.Fatalf("MetricsExporter = %q, want %q", cfg.MetricsExporter, config.MetricsExporterStdout)
	}
	if cfg.OTELExporterOTLPMetricsEndpoint != "https://collector.example/v1/metrics" {
		t.Fatalf("OTELExporterOTLPMetricsEndpoint = %q", cfg.OTELExporterOTLPMetricsEndpoint)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
