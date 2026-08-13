package sandboxmetadata

import (
	"testing"
	"time"
)

func TestTestOwnershipMetadata(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	got := TestOwnershipMetadata(map[string]string{
		KeyEnvironment:  "staging",
		KeyRegion:       "us-central1",
		KeyCanaryTarget: "staging-us-central1",
	}, TestOwnership{
		TestType:  TestTypeLoadTest,
		RunID:     "run-123",
		WorkerID:  "worker-7",
		CreatedAt: now,
		ExpiresAt: now.Add(2 * time.Hour),
	})

	want := map[string]string{
		KeyManagedBy:    ManagedByTestRunner,
		KeyEnvironment:  "staging",
		KeyRegion:       "us-central1",
		KeyCanaryTarget: "staging-us-central1",
		KeyTestType:     TestTypeLoadTest,
		KeyRunID:        "run-123",
		KeyWorkerID:     "worker-7",
		KeyCreatedAt:    now.Format(time.RFC3339),
		KeyExpiresAt:    now.Add(2 * time.Hour).Format(time.RFC3339),
	}
	for key, wantValue := range want {
		if gotValue := got[key]; gotValue != wantValue {
			t.Fatalf("%s = %q, want %q", key, gotValue, wantValue)
		}
	}
}

func TestLegacyCanaryMetadataAndRetention(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	retainedAt := now.Add(30 * time.Minute)
	expiresAt := retainedAt.Add(2 * time.Hour)

	got := LegacyCanaryMetadata("staging", "us-central1", "staging-us-central1", "run-123", now, now.Add(2*time.Hour))
	if got[KeyManagedBy] != ManagedByCanaryLegacy {
		t.Fatalf("managed_by = %q", got[KeyManagedBy])
	}
	if got[KeyCanaryTarget] != "staging-us-central1" {
		t.Fatalf("canary_target = %q", got[KeyCanaryTarget])
	}
	if got[KeyCreatedAt] != now.Format(time.RFC3339) {
		t.Fatalf("created_at = %q", got[KeyCreatedAt])
	}
	if got[KeyExpiresAt] != now.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("expires_at = %q", got[KeyExpiresAt])
	}

	retained := LegacyCanaryRetentionMetadata("staging", "us-central1", "staging-us-central1", "run-123", "initial_command", now, retainedAt, expiresAt)
	if retained[KeyRetainedForDebug] != "true" {
		t.Fatalf("retained_for_debug = %q", retained[KeyRetainedForDebug])
	}
	if retained[KeyFailedStep] != "initial_command" {
		t.Fatalf("failed_step = %q", retained[KeyFailedStep])
	}
	if retained[KeyRetainedAt] != retainedAt.Format(time.RFC3339) {
		t.Fatalf("retained_at = %q", retained[KeyRetainedAt])
	}
}

func TestMatchOwnershipAndStaleSince(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	canaryMetadata := map[string]string{
		KeyManagedBy:        ManagedByCanaryLegacy,
		KeyEnvironment:      "staging",
		KeyRetainedForDebug: "true",
		KeyCreatedAt:        now.Add(-3 * time.Hour).Format(time.RFC3339),
	}
	match, ok := MatchOwnership(canaryMetadata, "staging")
	if !ok {
		t.Fatal("expected canary metadata to match")
	}
	if !match.LegacyCanary || match.TestType != TestTypeCanary {
		t.Fatalf("unexpected canary match: %+v", match)
	}
	if staleSince, stale := StaleSince(canaryMetadata, match, now, 2*time.Hour); !stale || staleSince <= 0 {
		t.Fatalf("expected stale canary metadata, got stale=%v staleSince=%s", stale, staleSince)
	}

	loadRunnerMetadata := map[string]string{
		KeyManagedBy:   ManagedByTestRunner,
		KeyEnvironment: "staging",
		KeyTestType:    TestTypeLoadTest,
		KeyRunID:       "run-456",
		KeyCreatedAt:   now.Add(-3 * time.Hour).Format(time.RFC3339),
		KeyExpiresAt:   now.Add(-time.Hour).Format(time.RFC3339),
	}
	match, ok = MatchOwnership(loadRunnerMetadata, "staging")
	if !ok {
		t.Fatal("expected load runner metadata to match")
	}
	if match.LegacyCanary || match.TestType != TestTypeLoadTest {
		t.Fatalf("unexpected load runner match: %+v", match)
	}
	if staleSince, stale := StaleSince(loadRunnerMetadata, match, now, 2*time.Hour); !stale || staleSince <= 0 {
		t.Fatalf("expected stale load runner metadata, got stale=%v staleSince=%s", stale, staleSince)
	}

	if _, ok := MatchOwnership(map[string]string{KeyManagedBy: "customer", KeyEnvironment: "staging"}, "staging"); ok {
		t.Fatal("did not expect customer metadata to match")
	}

	if _, ok := MatchOwnership(map[string]string{
		KeyManagedBy:   ManagedByTestRunner,
		KeyEnvironment: "staging",
		KeyTestType:    TestTypeLoadTest,
		KeyCreatedAt:   now.Add(-time.Hour).Format(time.RFC3339),
	}, "staging"); ok {
		t.Fatal("did not expect incomplete generalized metadata to match")
	}

	if _, ok := MatchOwnership(map[string]string{
		KeyManagedBy:   ManagedByTestRunner,
		KeyEnvironment: "staging",
		KeyTestType:    "future-type",
		KeyRunID:       "run-999",
		KeyCreatedAt:   now.Add(-time.Hour).Format(time.RFC3339),
		KeyExpiresAt:   now.Add(-time.Minute).Format(time.RFC3339),
	}, "staging"); ok {
		t.Fatal("did not expect unknown generalized test_type to match")
	}

	if _, ok := MatchOwnership(map[string]string{
		KeyManagedBy:   ManagedByTestRunner,
		KeyEnvironment: "staging",
		KeyTestType:    TestTypeLoadTest,
		KeyRunID:       "run-789",
		KeyCreatedAt:   "not-a-timestamp",
		KeyExpiresAt:   now.Add(-time.Minute).Format(time.RFC3339),
	}, "staging"); ok {
		t.Fatal("did not expect malformed created_at to match")
	}

	if _, ok := MatchOwnership(map[string]string{
		KeyManagedBy:   ManagedByTestRunner,
		KeyEnvironment: "staging",
		KeyTestType:    TestTypeLoadTest,
		KeyRunID:       "run-789",
		KeyCreatedAt:   now.Add(-time.Hour).Format(time.RFC3339),
		KeyExpiresAt:   "not-a-timestamp",
	}, "staging"); ok {
		t.Fatal("did not expect malformed expires_at to match")
	}
}
