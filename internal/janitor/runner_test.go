package janitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

func TestJanitorDeletesExpiredLegacyRetainedSandbox(t *testing.T) {
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
						sandboxmetadata.KeyManagedBy:        sandboxmetadata.ManagedByCanaryLegacy,
						sandboxmetadata.KeyEnvironment:      "staging",
						sandboxmetadata.KeyRetainedForDebug: "true",
						sandboxmetadata.KeyCreatedAt:        now.Add(-3 * time.Hour).Format(time.RFC3339),
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

func TestJanitorPreservesUnexpiredManagedSandbox(t *testing.T) {
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
						sandboxmetadata.KeyManagedBy:   sandboxmetadata.ManagedByCanaryLegacy,
						sandboxmetadata.KeyEnvironment: "staging",
						sandboxmetadata.KeyExpiresAt:   now.Add(time.Hour).Format(time.RFC3339),
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

func TestJanitorSkipsUnownedSandboxWithCoincidentalMetadata(t *testing.T) {
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
						sandboxmetadata.KeyManagedBy:   "customer",
						sandboxmetadata.KeyEnvironment: "staging",
						sandboxmetadata.KeyTestType:    sandboxmetadata.TestTypeLoadTest,
						sandboxmetadata.KeyRunID:       "run-coincidental",
						sandboxmetadata.KeyCreatedAt:   now.Add(-2 * time.Hour).Format(time.RFC3339),
						sandboxmetadata.KeyExpiresAt:   now.Add(-time.Hour).Format(time.RFC3339),
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

func TestJanitorSkipsSandboxSpoofingGenericFieldsWithoutTrustedOwner(t *testing.T) {
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
						sandboxmetadata.KeyEnvironment: "staging",
						sandboxmetadata.KeyTestType:    sandboxmetadata.TestTypeLoadTest,
						sandboxmetadata.KeyRunID:       "run-spoofed",
						sandboxmetadata.KeyCreatedAt:   now.Add(-2 * time.Hour).Format(time.RFC3339),
						sandboxmetadata.KeyExpiresAt:   now.Add(-time.Hour).Format(time.RFC3339),
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
						"managed_by":  "api-canary",
						"environment": "staging",
						"expires_at":  now.Add(-1 * time.Hour).Format(time.RFC3339),
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

func TestJanitorRecognizesLegacyAndGeneralizedOwnedSandboxes(t *testing.T) {
	now := time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)
	var queries []string
	deleted := map[string]bool{}
	r := Runner{
		Config: config.Config{
			Environment:            "staging",
			Region:                 "us-central1",
			Target:                 "staging-us-central1",
			RetainFailedSandboxTTL: 2 * time.Hour,
		},
		Client: &fakeJanitorClient{
			listSandboxesFn: func(_ context.Context, query map[string]string) ([]canaryapi.Sandbox, error) {
				queries = append(queries, query["metadata.managed_by"])
				if got := query["metadata.environment"]; got != "staging" {
					t.Fatalf("unexpected environment query %q", got)
				}
				switch query["metadata.managed_by"] {
				case sandboxmetadata.ManagedByCanaryLegacy:
					return []canaryapi.Sandbox{{
						ID: "sb-canary",
						Metadata: map[string]string{
							sandboxmetadata.KeyManagedBy:   sandboxmetadata.ManagedByCanaryLegacy,
							sandboxmetadata.KeyEnvironment: "staging",
							sandboxmetadata.KeyExpiresAt:   now.Add(-1 * time.Hour).Format(time.RFC3339),
						},
					}}, nil
				case sandboxmetadata.ManagedByTestRunner:
					return []canaryapi.Sandbox{{
						ID: "sb-load",
						Metadata: sandboxmetadata.TestOwnershipMetadata(map[string]string{
							sandboxmetadata.KeyEnvironment: "staging",
							sandboxmetadata.KeyRegion:      "us-central1",
						}, sandboxmetadata.TestOwnership{
							TestType:  sandboxmetadata.TestTypeLoadTest,
							RunID:     "run-load-1",
							WorkerID:  "worker-2",
							CreatedAt: now.Add(-2 * time.Hour),
							ExpiresAt: now.Add(-1 * time.Hour),
						}),
					}}, nil
				default:
					t.Fatalf("unexpected managed_by query %q", query["metadata.managed_by"])
				}
				return nil, nil
			},
			deleteSandboxFn: func(_ context.Context, id string) error {
				deleted[id] = true
				return nil
			},
		},
		Metrics: &janitorMetricsRecorder{},
		Clock:   func() time.Time { return now },
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("expected two ownership queries, got %v", queries)
	}
	if queries[0] != sandboxmetadata.ManagedByCanaryLegacy || queries[1] != sandboxmetadata.ManagedByTestRunner {
		t.Fatalf("unexpected ownership queries: %v", queries)
	}
	if !deleted["sb-canary"] || !deleted["sb-load"] {
		t.Fatalf("expected both sandboxes to be deleted, got %v", deleted)
	}
}

func TestJanitorMinimumSafetyMatrix(t *testing.T) {
	now := time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)
	deleted := map[string]int{}
	r := Runner{
		Config: config.Config{
			Environment:            "staging",
			Region:                 "us-central1",
			Target:                 "staging-us-central1",
			RetainFailedSandboxTTL: 2 * time.Hour,
		},
		Client: &fakeJanitorClient{
			listSandboxesFn: func(_ context.Context, query map[string]string) ([]canaryapi.Sandbox, error) {
				switch query["metadata.managed_by"] {
				case sandboxmetadata.ManagedByCanaryLegacy:
					return []canaryapi.Sandbox{{
						ID: "legacy-expired",
						Metadata: map[string]string{
							sandboxmetadata.KeyManagedBy:   sandboxmetadata.ManagedByCanaryLegacy,
							sandboxmetadata.KeyEnvironment: "staging",
							sandboxmetadata.KeyExpiresAt:   now.Add(-time.Hour).Format(time.RFC3339),
						},
					}}, nil
				case sandboxmetadata.ManagedByTestRunner:
					return []canaryapi.Sandbox{
						{
							ID: "load-expired",
							Metadata: sandboxmetadata.TestOwnershipMetadata(map[string]string{
								sandboxmetadata.KeyEnvironment: "staging",
								sandboxmetadata.KeyRegion:      "us-central1",
							}, sandboxmetadata.TestOwnership{
								TestType:  sandboxmetadata.TestTypeLoadTest,
								RunID:     "run-expired",
								WorkerID:  "worker-a",
								CreatedAt: now.Add(-2 * time.Hour),
								ExpiresAt: now.Add(-time.Hour),
							}),
						},
						{
							ID: "load-fresh",
							Metadata: sandboxmetadata.TestOwnershipMetadata(map[string]string{
								sandboxmetadata.KeyEnvironment: "staging",
								sandboxmetadata.KeyRegion:      "us-central1",
							}, sandboxmetadata.TestOwnership{
								TestType:  sandboxmetadata.TestTypeLoadTest,
								RunID:     "run-fresh",
								CreatedAt: now.Add(-time.Hour),
								ExpiresAt: now.Add(time.Hour),
							}),
						},
						{
							ID: "customer-coincidental",
							Metadata: map[string]string{
								sandboxmetadata.KeyManagedBy:   "customer",
								sandboxmetadata.KeyEnvironment: "staging",
								sandboxmetadata.KeyTestType:    sandboxmetadata.TestTypeLoadTest,
								sandboxmetadata.KeyRunID:       "run-customer",
								sandboxmetadata.KeyCreatedAt:   now.Add(-2 * time.Hour).Format(time.RFC3339),
								sandboxmetadata.KeyExpiresAt:   now.Add(-time.Hour).Format(time.RFC3339),
							},
						},
					}, nil
				default:
					return nil, nil
				}
			},
			deleteSandboxFn: func(_ context.Context, id string) error {
				deleted[id]++
				return nil
			},
		},
		Metrics: &janitorMetricsRecorder{},
		Clock:   func() time.Time { return now },
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("first run returned %v", err)
	}
	if deleted["legacy-expired"] != 1 || deleted["load-expired"] != 1 {
		t.Fatalf("expected expired owned sandboxes to be deleted once, got %v", deleted)
	}
	if deleted["load-fresh"] != 0 || deleted["customer-coincidental"] != 0 {
		t.Fatalf("expected fresh/customer sandboxes to be preserved, got %v", deleted)
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("second run returned %v", err)
	}
	if deleted["legacy-expired"] != 2 || deleted["load-expired"] != 2 {
		t.Fatalf("expected repeated janitor pass to remain harmless, got %v", deleted)
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
func (m *janitorMetricsRecorder) RecordOrphans(context.Context, string, string, string, int64, time.Duration) {
}
func (m *janitorMetricsRecorder) RecordRetainedSandbox(context.Context, string, string, string, string) {
}
func (m *janitorMetricsRecorder) RecordJanitorResources(_ context.Context, _ string, _ string, _ string, examined, deleted, deleteFailures int64) {
	m.examined += examined
	m.deleted += deleted
	m.deleteFailures += deleteFailures
}
