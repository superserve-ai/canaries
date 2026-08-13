package sandboxmetadata

import "time"

const (
	ManagedByCanaryLegacy = "api-canary"
	ManagedByTestRunner   = "superserve-test-runner"

	TestTypeCanary   = "canary"
	TestTypeLoadTest = "loadtest"

	KeyManagedBy        = "managed_by"
	KeyEnvironment      = "environment"
	KeyRegion           = "region"
	KeyCanaryTarget     = "canary_target"
	KeyTestType         = "test_type"
	KeyRunID            = "run_id"
	KeyWorkerID         = "worker_id"
	KeyCreatedAt        = "created_at"
	KeyExpiresAt        = "expires_at"
	KeyRetainedForDebug = "retained_for_debug"
	KeyFailedStep       = "failed_step"
	KeyRetainedAt       = "retained_at"
)

type TestOwnership struct {
	TestType  string
	RunID     string
	WorkerID  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type OwnershipMatch struct {
	ManagedBy    string
	TestType     string
	LegacyCanary bool
}

var knownTestTypes = map[string]struct{}{
	TestTypeCanary:   {},
	TestTypeLoadTest: {},
}

func RecognizedManagedByValues() []string {
	return []string{ManagedByCanaryLegacy, ManagedByTestRunner}
}

func OwnershipQuery(environment, managedBy string) map[string]string {
	return map[string]string{
		"metadata." + KeyManagedBy:   managedBy,
		"metadata." + KeyEnvironment: environment,
	}
}

func LegacyCanaryMetadata(environment, region, canaryTarget, runID string, createdAt, expiresAt time.Time) map[string]string {
	return map[string]string{
		KeyManagedBy:    ManagedByCanaryLegacy,
		KeyEnvironment:  environment,
		KeyRegion:       region,
		KeyCanaryTarget: canaryTarget,
		KeyCreatedAt:    createdAt.Format(time.RFC3339),
		KeyExpiresAt:    expiresAt.Format(time.RFC3339),
		KeyRunID:        runID,
	}
}

func LegacyCanaryRetentionMetadata(environment, region, canaryTarget, runID, failedStep string, createdAt, retainedAt, expiresAt time.Time) map[string]string {
	return map[string]string{
		KeyManagedBy:        ManagedByCanaryLegacy,
		KeyCanaryTarget:     canaryTarget,
		KeyEnvironment:      environment,
		KeyRegion:           region,
		KeyRunID:            runID,
		KeyCreatedAt:        createdAt.Format(time.RFC3339),
		KeyRetainedForDebug: "true",
		KeyFailedStep:       failedStep,
		KeyRetainedAt:       retainedAt.Format(time.RFC3339),
		KeyExpiresAt:        expiresAt.Format(time.RFC3339),
	}
}

func TestOwnershipMetadata(base map[string]string, ownership TestOwnership) map[string]string {
	out := clone(base)
	out[KeyManagedBy] = ManagedByTestRunner
	out[KeyTestType] = ownership.TestType
	out[KeyRunID] = ownership.RunID
	out[KeyCreatedAt] = ownership.CreatedAt.UTC().Format(time.RFC3339)
	out[KeyExpiresAt] = ownership.ExpiresAt.UTC().Format(time.RFC3339)
	if ownership.WorkerID != "" {
		out[KeyWorkerID] = ownership.WorkerID
	}
	return out
}

func MatchOwnership(metadata map[string]string, environment string) (OwnershipMatch, bool) {
	if metadata == nil || metadata[KeyEnvironment] != environment {
		return OwnershipMatch{}, false
	}

	switch metadata[KeyManagedBy] {
	case ManagedByCanaryLegacy:
		return OwnershipMatch{
			ManagedBy:    ManagedByCanaryLegacy,
			TestType:     TestTypeCanary,
			LegacyCanary: true,
		}, true
	case ManagedByTestRunner:
		if !isKnownTestType(metadata[KeyTestType]) || metadata[KeyRunID] == "" {
			return OwnershipMatch{}, false
		}
		if _, ok := parseTime(metadata[KeyCreatedAt]); !ok {
			return OwnershipMatch{}, false
		}
		if _, ok := parseTime(metadata[KeyExpiresAt]); !ok {
			return OwnershipMatch{}, false
		}
		return OwnershipMatch{
			ManagedBy: ManagedByTestRunner,
			TestType:  metadata[KeyTestType],
		}, true
	default:
		return OwnershipMatch{}, false
	}
}

func StaleSince(metadata map[string]string, match OwnershipMatch, now time.Time, retainTTL time.Duration) (time.Duration, bool) {
	if expiresAt, ok := parseTime(metadata[KeyExpiresAt]); ok {
		if expiresAt.After(now) {
			return 0, false
		}
		return now.Sub(expiresAt), true
	}

	if !match.LegacyCanary || metadata[KeyRetainedForDebug] != "true" {
		return 0, false
	}

	createdAt, ok := parseTime(metadata[KeyCreatedAt])
	if !ok {
		return 0, false
	}
	staleSince := createdAt.Add(retainTTL)
	if staleSince.After(now) {
		return 0, false
	}
	return now.Sub(staleSince), true
}

func parseTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

func isKnownTestType(testType string) bool {
	_, ok := knownTestTypes[testType]
	return ok
}

func clone(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
