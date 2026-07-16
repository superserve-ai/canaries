package janitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
)

func TestJanitorDeletesExpiredRetainedSandbox(t *testing.T) {
	now := time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)
	deleteCalled := false
	metrics := &janitorMetricsRecorder{}
	r := Runner{
		Config: config.Config{
			Environment:            "staging",
			Region:                 "us-central1",
			Target:                 "staging-us-central1",
			RetainFailedSandboxTTL: 2 * time.Hour,
		},
		Client: &fakeJanitorClient{
			listSandboxesFn: func(context.Context, map[string]string) ([]canaryapi.Sandbox, error) {
				return []canaryapi.Sandbox{{
					ID: "sb-1",
					Metadata: map[string]string{
						"managed_by":         "api-canary",
						"retained_for_debug": "true",
						"created_at":         now.Add(-3 * time.Hour).Format(time.RFC3339),
					},
				}}, nil
			},
			deleteSandboxFn: func(context.Context, string) error {
				deleteCalled = true
				return nil
			},
		},
		Metrics: metrics,
		Clock:   func() time.Time { return now },
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if !deleteCalled {
		t.Fatal("expected delete")
	}
	if metrics.examined != 1 || metrics.deleted != 1 || metrics.deleteFailures != 0 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
}

func TestJanitorPreservesUnexpiredRetainedSandbox(t *testing.T) {
	now := time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)
	deleteCalled := false
	r := Runner{
		Config: config.Config{
			Environment:            "staging",
			Region:                 "us-central1",
			Target:                 "staging-us-central1",
			RetainFailedSandboxTTL: 2 * time.Hour,
		},
		Client: &fakeJanitorClient{
			listSandboxesFn: func(context.Context, map[string]string) ([]canaryapi.Sandbox, error) {
				return []canaryapi.Sandbox{{
					ID: "sb-1",
					Metadata: map[string]string{
						"managed_by":         "api-canary",
						"retained_for_debug": "true",
						"created_at":         now.Add(-1 * time.Hour).Format(time.RFC3339),
					},
				}}, nil
			},
			deleteSandboxFn: func(context.Context, string) error {
				deleteCalled = true
				return nil
			},
		},
		Metrics: &janitorMetricsRecorder{},
		Clock:   func() time.Time { return now },
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if deleteCalled {
		t.Fatal("did not expect delete")
	}
}

func TestJanitorSkipsNonCanarySandbox(t *testing.T) {
	now := time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)
	deleteCalled := false
	r := Runner{
		Config: config.Config{
			Environment:            "staging",
			Region:                 "us-central1",
			Target:                 "staging-us-central1",
			RetainFailedSandboxTTL: 2 * time.Hour,
		},
		Client: &fakeJanitorClient{
			listSandboxesFn: func(context.Context, map[string]string) ([]canaryapi.Sandbox, error) {
				return []canaryapi.Sandbox{{
					ID: "sb-1",
					Metadata: map[string]string{
						"managed_by": "customer",
						"expires_at": now.Add(-1 * time.Hour).Format(time.RFC3339),
					},
				}}, nil
			},
			deleteSandboxFn: func(context.Context, string) error {
				deleteCalled = true
				return nil
			},
		},
		Metrics: &janitorMetricsRecorder{},
		Clock:   func() time.Time { return now },
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if deleteCalled {
		t.Fatal("did not expect delete")
	}
}

func TestJanitorDeleteFailureIsRecorded(t *testing.T) {
	now := time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)
	metrics := &janitorMetricsRecorder{}
	r := Runner{
		Config: config.Config{
			Environment:            "staging",
			Region:                 "us-central1",
			Target:                 "staging-us-central1",
			RetainFailedSandboxTTL: 2 * time.Hour,
		},
		Client: &fakeJanitorClient{
			listSandboxesFn: func(context.Context, map[string]string) ([]canaryapi.Sandbox, error) {
				return []canaryapi.Sandbox{{
					ID: "sb-1",
					Metadata: map[string]string{
						"managed_by": "api-canary",
						"expires_at": now.Add(-1 * time.Hour).Format(time.RFC3339),
					},
				}}, nil
			},
			deleteSandboxFn: func(context.Context, string) error {
				return errors.New("delete failed")
			},
		},
		Metrics: metrics,
		Clock:   func() time.Time { return now },
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if metrics.deleteFailures != 1 {
		t.Fatalf("expected one delete failure, got %+v", metrics)
	}
}

type fakeJanitorClient struct {
	listSandboxesFn func(context.Context, map[string]string) ([]canaryapi.Sandbox, error)
	deleteSandboxFn func(context.Context, string) error
}

func (f *fakeJanitorClient) ListSandboxes(ctx context.Context, query map[string]string) ([]canaryapi.Sandbox, error) {
	return f.listSandboxesFn(ctx, query)
}

func (f *fakeJanitorClient) DeleteSandbox(ctx context.Context, id string) error {
	if f.deleteSandboxFn == nil {
		return nil
	}
	return f.deleteSandboxFn(ctx, id)
}

type janitorMetricsRecorder struct {
	examined       int64
	deleted        int64
	deleteFailures int64
}

func (m *janitorMetricsRecorder) RecordRun(context.Context, string, string, string, string, string, time.Duration) {
}
func (m *janitorMetricsRecorder) RecordStep(context.Context, string, string, string, string, string, string, time.Duration) {
}
func (m *janitorMetricsRecorder) RecordCleanup(context.Context, string, string, string, string) {}
func (m *janitorMetricsRecorder) RecordOverlapSkip(context.Context, string, string, string)     {}
func (m *janitorMetricsRecorder) RecordExecutionDelta(context.Context, string, string, string, string, int64) {
}
func (m *janitorMetricsRecorder) RecordOrphans(context.Context, string, string, string, int64, time.Duration) {}
func (m *janitorMetricsRecorder) RecordRetainedSandbox(context.Context, string, string, string, string) {
}
func (m *janitorMetricsRecorder) RecordJanitorResources(_ context.Context, _ string, _ string, _ string, examined, deleted, deleteFailures int64) {
	m.examined += examined
	m.deleted += deleted
	m.deleteFailures += deleteFailures
}
