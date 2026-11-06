package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/config"
)

func TestRunRoutesSoakModeToPrivateSandboxScenario(t *testing.T) {
	for _, useFlag := range []bool{true, false} {
		name := "environment mode"
		if useFlag {
			name = "flag mode"
		}
		t.Run(name, func(t *testing.T) {
			requests := 0
			var payload struct {
				PreviewAccess string            `json:"preview_access"`
				Metadata      map[string]string `json:"metadata"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests++
				if req.Method != http.MethodPost || req.URL.Path != "/sandboxes" || req.Header.Get("X-API-Key") != "test-key" {
					t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
				}
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Errorf("invalid create request: %v", err)
				}
				// Fail before allocating a sandbox: this proves application routing
				// without running the six-hour scenario or touching real resources.
				http.Error(w, "soak fixture rejected creation", http.StatusInternalServerError)
			}))
			defer server.Close()
			for key, value := range map[string]string{
				"CANARY_TARGET": "staging-us-central1", "CANARY_ENVIRONMENT": "staging", "CANARY_REGION": "us-central1",
				"GCP_PROJECT_ID": "rayai-dev", "API_BASE_URL": server.URL, "PREVIEW_DOMAIN": "preview.example",
				"CANARY_API_KEY": "test-key", "CANARY_RUNTIME": "local", "CANARY_METRICS_EXPORTER": "none",
				"CANARY_LOCK_BACKEND": "none", "CANARY_SLACK_WEBHOOK_URL": "", "CANARY_MODE": "soak",
				"CANARY_SOAK_DURATION": "", "CANARY_SOAK_INTERVAL": "", "RUN_TIMEOUT": "", "LOCK_TTL": "", "RESOURCE_TTL": "",
				"COMMAND_TIMEOUT": "", "DELETE_TIMEOUT": "", "PREVIEW_TIMEOUT": "", "PREVIEW_PORT": "", "HTTP_TIMEOUT": "",
				"CANARY_RETAIN_FAILED_SANDBOX": "true", "CANARY_RETAIN_FAILED_SANDBOX_TTL": "", "ALLOW_STAGING_MUTATION": "false",
			} {
				t.Setenv(key, value)
			}
			var args []string
			if useFlag {
				// The explicit flag must take precedence over the environment.
				t.Setenv("CANARY_MODE", "lifecycle")
				args = []string{"-mode", "soak"}
			}
			if err := Run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "soak fixture rejected creation") {
				t.Fatalf("error = %v, want the soak create failure", err)
			}
			if requests != 1 || payload.PreviewAccess != "private" || payload.Metadata["scenario"] != "soak" {
				t.Fatalf("expected one private soak creation, got requests=%d access=%q scenario=%q", requests, payload.PreviewAccess, payload.Metadata["scenario"])
			}
		})
	}
}

func TestCombineRunAndShutdownError(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() {
		log.Logger = orig
	})

	t.Run("primary error preserved when shutdown fails", func(t *testing.T) {
		buf.Reset()
		runErr := errors.New("canary failed")
		shutdownErr := errors.New("metrics failed")
		got := combineRunAndShutdownError(runErr, shutdownErr, config.RuntimeCloudRun)
		if !errors.Is(got, runErr) {
			t.Fatalf("got %v, want primary error", got)
		}
		if !strings.Contains(buf.String(), "metrics shutdown failed") {
			t.Fatalf("expected shutdown log, got %q", buf.String())
		}
	})

	t.Run("cloud-run success ignores shutdown error", func(t *testing.T) {
		buf.Reset()
		shutdownErr := errors.New("metrics failed")
		got := combineRunAndShutdownError(nil, shutdownErr, config.RuntimeCloudRun)
		if got != nil {
			t.Fatalf("got %v, want nil", got)
		}
		if !strings.Contains(buf.String(), "metrics shutdown failed") {
			t.Fatalf("expected shutdown log, got %q", buf.String())
		}
	})

	t.Run("local success logs warning and returns nil", func(t *testing.T) {
		buf.Reset()
		shutdownErr := errors.New("metrics failed")
		got := combineRunAndShutdownError(nil, shutdownErr, config.RuntimeLocal)
		if got != nil {
			t.Fatalf("got %v, want nil", got)
		}
		if !strings.Contains(buf.String(), "metrics shutdown failed") {
			t.Fatalf("expected shutdown log, got %q", buf.String())
		}
	})
}
