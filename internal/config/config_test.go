package config

import (
	"testing"
	"time"
)

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
		if got, want := cfg.SandboxTemplate, "superserve/python-3.11"; got != want {
			t.Fatalf("sandbox template = %q, want %q", got, want)
		}
		if cfg.RetainFailedSandbox {
			t.Fatal("retain failed sandbox should default to false")
		}
		if got, want := cfg.RetainFailedSandboxTTL, 2*time.Hour; got != want {
			t.Fatalf("retain failed sandbox ttl = %s, want %s", got, want)
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

	t.Run("retention defaults to false", func(t *testing.T) {
		baseEnv()
		cfg, err := Load("lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RetainFailedSandbox {
			t.Fatal("expected retention to default false")
		}
	})

	t.Run("invalid retention bool rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RETAIN_FAILED_SANDBOX", "maybe")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("valid retention ttl accepted", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RETAIN_FAILED_SANDBOX", "true")
		t.Setenv("CANARY_RETAIN_FAILED_SANDBOX_TTL", "4h")
		cfg, err := Load("lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.RetainFailedSandbox {
			t.Fatal("expected retention enabled")
		}
		if got, want := cfg.RetainFailedSandboxTTL, 4*time.Hour; got != want {
			t.Fatalf("retain ttl = %s, want %s", got, want)
		}
	})

	t.Run("zero ttl rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RETAIN_FAILED_SANDBOX_TTL", "0s")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("negative ttl rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RETAIN_FAILED_SANDBOX_TTL", "-1s")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid retention duration rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RETAIN_FAILED_SANDBOX_TTL", "not-a-duration")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("excessive ttl rejected", func(t *testing.T) {
		baseEnv()
		t.Setenv("CANARY_RETAIN_FAILED_SANDBOX_TTL", "25h")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestLoadTargetValidation(t *testing.T) {
	uiBase := func() {
		t.Setenv("CANARY_TARGET", "staging-us-central1")
		t.Setenv("CANARY_ENVIRONMENT", "staging")
		t.Setenv("CANARY_REGION", "us-central1")
		t.Setenv("GCP_PROJECT_ID", "rayai-dev")
		t.Setenv("CANARY_RUNTIME", "local")
		t.Setenv("CANARY_METRICS_EXPORTER", "none")
		t.Setenv("CANARY_LOCK_BACKEND", "none")
	}

	t.Run("valid env-region tuple accepted", func(t *testing.T) {
		uiBase()
		if _, err := Load("ui-lifecycle"); err != nil {
			t.Fatalf("unexpected error for valid target: %v", err)
		}
	})

	t.Run("malformed target without hyphen rejected", func(t *testing.T) {
		uiBase()
		t.Setenv("CANARY_TARGET", "prod")
		if _, err := Load("ui-lifecycle"); err == nil {
			t.Fatal("expected error for malformed target 'prod'")
		}
	})

	t.Run("target environment part mismatch rejected", func(t *testing.T) {
		uiBase()
		t.Setenv("CANARY_TARGET", "production-us-central1")
		t.Setenv("CANARY_ENVIRONMENT", "staging")
		t.Setenv("CANARY_REGION", "us-central1")
		if _, err := Load("ui-lifecycle"); err == nil {
			t.Fatal("expected error when target environment part mismatches CANARY_ENVIRONMENT")
		}
	})

	t.Run("target region part mismatch rejected", func(t *testing.T) {
		uiBase()
		t.Setenv("CANARY_TARGET", "staging-us-east4")
		t.Setenv("CANARY_ENVIRONMENT", "staging")
		t.Setenv("CANARY_REGION", "us-central1")
		if _, err := Load("ui-lifecycle"); err == nil {
			t.Fatal("expected error when target region part mismatches CANARY_REGION")
		}
	})

	t.Run("lifecycle mode also validates target format", func(t *testing.T) {
		t.Setenv("CANARY_TARGET", "invalid-target")
		t.Setenv("CANARY_ENVIRONMENT", "staging")
		t.Setenv("CANARY_REGION", "us-central1")
		t.Setenv("GCP_PROJECT_ID", "rayai-dev")
		t.Setenv("API_BASE_URL", "https://api-staging.superserve.ai")
		t.Setenv("PREVIEW_DOMAIN", "staging-sandbox.superserve.ai")
		t.Setenv("CANARY_API_KEY", "ss_test")
		t.Setenv("CANARY_RUNTIME", "local")
		t.Setenv("CANARY_METRICS_EXPORTER", "none")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected lifecycle mode to reject invalid target format")
		}
	})
}

func TestUILifecycleCloudRunLockExemption(t *testing.T) {
	base := func() {
		t.Setenv("CANARY_TARGET", "staging-us-central1")
		t.Setenv("CANARY_ENVIRONMENT", "staging")
		t.Setenv("CANARY_REGION", "us-central1")
		t.Setenv("GCP_PROJECT_ID", "rayai-dev")
		t.Setenv("CANARY_RUNTIME", "cloud-run")
		t.Setenv("CANARY_METRICS_EXPORTER", "otlp")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "https://collector.example/v1/metrics")
	}

	t.Run("ui-lifecycle on cloud-run with lock=none accepted", func(t *testing.T) {
		base()
		t.Setenv("CANARY_LOCK_BACKEND", "none")
		if _, err := Load("ui-lifecycle"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("lifecycle on cloud-run with lock=none still rejected", func(t *testing.T) {
		base()
		t.Setenv("CANARY_LOCK_BACKEND", "none")
		t.Setenv("API_BASE_URL", "https://api-staging.superserve.ai")
		t.Setenv("PREVIEW_DOMAIN", "staging-sandbox.superserve.ai")
		t.Setenv("CANARY_API_KEY", "ss_test")
		if _, err := Load("lifecycle"); err == nil {
			t.Fatal("expected error: lifecycle on cloud-run requires gcs lock")
		}
	})
}
