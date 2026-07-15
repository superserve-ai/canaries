package config

import "testing"

func TestLoad(t *testing.T) {
	baseEnv := func() {
		t.Setenv("CANARY_TARGET", "staging-us-central1")
		t.Setenv("CANARY_ENVIRONMENT", "staging")
		t.Setenv("CANARY_REGION", "us-central1")
		t.Setenv("GCP_PROJECT_ID", "rayai-dev")
		t.Setenv("API_BASE_URL", "https://api-staging.superserve.ai")
		t.Setenv("PREVIEW_DOMAIN", "staging-sandbox.superserve.ai")
		t.Setenv("CANARY_API_KEY", "ss_test")
	}

	t.Run("local defaults", func(t *testing.T) {
		baseEnv()
		cfg, err := Load("lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Runtime != RuntimeLocal {
			t.Fatalf("runtime = %q", cfg.Runtime)
		}
		if cfg.MetricsExporter != MetricsExporterNone {
			t.Fatalf("metrics exporter = %q", cfg.MetricsExporter)
		}
		if cfg.LockBackend != LockBackendFile {
			t.Fatalf("lock backend = %q", cfg.LockBackend)
		}
		if got, want := cfg.LockFile, "/tmp/superserve-canary-staging-us-central1.lock"; got != want {
			t.Fatalf("lock file = %q, want %q", got, want)
		}
	})

	t.Run("cloud-run defaults", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RUNTIME", "cloud-run")
		t.Setenv("LOCK_BUCKET", "rayai-dev-api-canary-locks")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector.example/v1/metrics")
		cfg, err := Load("lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Runtime != RuntimeCloudRun {
			t.Fatalf("runtime = %q", cfg.Runtime)
		}
		if cfg.MetricsExporter != MetricsExporterOTLP {
			t.Fatalf("metrics exporter = %q", cfg.MetricsExporter)
		}
		if cfg.LockBackend != LockBackendGCS {
			t.Fatalf("lock backend = %q", cfg.LockBackend)
		}
	})

	t.Run("unknown exporter rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_METRICS_EXPORTER", "bogus")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("unknown lock backend rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_LOCK_BACKEND", "bogus")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("cloud-run with none metrics rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RUNTIME", "cloud-run")
		t.Setenv("CANARY_METRICS_EXPORTER", "none")
		t.Setenv("CANARY_LOCK_BACKEND", "gcs")
		t.Setenv("LOCK_BUCKET", "rayai-dev-api-canary-locks")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector.example/v1/metrics")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("cloud-run with file lock rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RUNTIME", "cloud-run")
		t.Setenv("CANARY_METRICS_EXPORTER", "otlp")
		t.Setenv("CANARY_LOCK_BACKEND", "file")
		t.Setenv("LOCK_BUCKET", "rayai-dev-api-canary-locks")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector.example/v1/metrics")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("cloud-run without otlp endpoint rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RUNTIME", "cloud-run")
		t.Setenv("CANARY_METRICS_EXPORTER", "otlp")
		t.Setenv("CANARY_LOCK_BACKEND", "gcs")
		t.Setenv("LOCK_BUCKET", "rayai-dev-api-canary-locks")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("cloud-run with generic otlp endpoint accepted", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RUNTIME", "cloud-run")
		t.Setenv("CANARY_METRICS_EXPORTER", "otlp")
		t.Setenv("CANARY_LOCK_BACKEND", "gcs")
		t.Setenv("LOCK_BUCKET", "rayai-dev-api-canary-locks")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example/v1/metrics")
		cfg, err := Load("lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OTELExporterOTLPMetricsEndpoint == "" {
			t.Fatal("endpoint not populated")
		}
	})

	t.Run("local with none accepted", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RUNTIME", "local")
		t.Setenv("CANARY_METRICS_EXPORTER", "none")
		t.Setenv("CANARY_LOCK_BACKEND", "none")
		cfg, err := Load("lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.LockBackend != LockBackendNone {
			t.Fatalf("lock backend = %q", cfg.LockBackend)
		}
	})

	t.Run("local with file accepted", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RUNTIME", "local")
		t.Setenv("CANARY_METRICS_EXPORTER", "none")
		t.Setenv("CANARY_LOCK_BACKEND", "file")
		cfg, err := Load("lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.LockBackend != LockBackendFile {
			t.Fatalf("lock backend = %q", cfg.LockBackend)
		}
	})

	t.Run("local with gcs accepted", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RUNTIME", "local")
		t.Setenv("CANARY_METRICS_EXPORTER", "none")
		t.Setenv("CANARY_LOCK_BACKEND", "gcs")
		t.Setenv("LOCK_BUCKET", "rayai-dev-api-canary-locks")
		cfg, err := Load("lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.LockBackend != LockBackendGCS {
			t.Fatalf("lock backend = %q", cfg.LockBackend)
		}
	})
}
