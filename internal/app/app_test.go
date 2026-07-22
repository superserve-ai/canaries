package app

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/config"
)

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
