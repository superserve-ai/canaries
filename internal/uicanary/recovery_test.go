package uicanary

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

type mockRecovery struct {
	list   func(context.Context, map[string]string) ([]canaryapi.Sandbox, error)
	delete func(context.Context, string) error
}

func (m mockRecovery) ListSandboxes(ctx context.Context, query map[string]string) ([]canaryapi.Sandbox, error) {
	return m.list(ctx, query)
}

func (m mockRecovery) DeleteSandbox(ctx context.Context, id string) error {
	return m.delete(ctx, id)
}

func TestMissingBrowserIDRecoversCreatedSandbox(t *testing.T) {
	skipIfPlaywrightUnavailable(t)
	state := &mockServerState{hideCreatedID: true}
	server := setupMockConsoleServer(state)
	defer server.Close()
	runner := Runner{Config: newMockRunnerConfig(server.URL, ""), Clock: time.Now}
	runner.Config.StepTimeout = time.Second
	deleted := false
	runner.Recovery = mockRecovery{
		list: func(ctx context.Context, query map[string]string) ([]canaryapi.Sandbox, error) {
			if ctx.Err() != nil {
				t.Fatalf("recovery context already expired: %v", ctx.Err())
			}
			state.Lock()
			defer state.Unlock()
			payload := state.createdPayload
			if payload == nil {
				t.Fatal("UI never submitted the create request")
			}
			if payload["timeout_seconds"] != float64(330) || payload["auto_delete_seconds"] != float64(300) {
				t.Fatalf("create request lacks safety settings: %v", payload)
			}
			metadata := map[string]string{}
			for key, value := range payload["metadata"].(map[string]any) {
				metadata[key] = value.(string)
			}
			for _, key := range []string{sandboxmetadata.KeyManagedBy, sandboxmetadata.KeyEnvironment, sandboxmetadata.KeyCanaryTarget, sandboxmetadata.KeyRunID} {
				if metadata[key] == "" || query["metadata."+key] != metadata[key] {
					t.Fatalf("missing creation ownership or query filter %s: %v, %v", key, metadata, query)
				}
			}
			return []canaryapi.Sandbox{{ID: "orphan-id", Name: payload["name"].(string), Metadata: metadata}}, nil
		},
		delete: func(ctx context.Context, id string) error {
			if id != "orphan-id" {
				t.Fatalf("deleted wrong sandbox: %s", id)
			}
			deleted = true
			return nil
		},
	}
	if err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "could not extract sandbox ID") {
		t.Fatalf("creation failure must remain visible, got %v", err)
	}
	if !deleted {
		t.Fatal("sandbox created without a browser ID was not recovered")
	}
}

func TestRecoveryOnlyDeletesExactRunOwnership(t *testing.T) {
	runner := Runner{Config: newMockRunnerConfig("http://example.test", ""), Clock: time.Now}
	metadata := runner.creationMetadata("run-owned")
	items := []canaryapi.Sandbox{{ID: "owned", Name: "owned-name", Metadata: metadata}}
	for _, key := range []string{sandboxmetadata.KeyEnvironment, sandboxmetadata.KeyCanaryTarget, sandboxmetadata.KeyRunID, sandboxmetadata.KeyRegion, sandboxmetadata.KeyManagedBy} {
		other := map[string]string{}
		for k, v := range metadata {
			other[k] = v
		}
		other[key] = "other"
		items = append(items, canaryapi.Sandbox{ID: "wrong-" + key, Name: "owned-name", Metadata: other})
	}
	items = append(items, canaryapi.Sandbox{ID: "untagged", Name: "owned-name"}, canaryapi.Sandbox{ID: "wrong-name", Name: "other", Metadata: metadata})
	var deleted []string
	runner.Recovery = mockRecovery{
		list:   func(context.Context, map[string]string) ([]canaryapi.Sandbox, error) { return items, nil },
		delete: func(_ context.Context, id string) error { deleted = append(deleted, id); return nil },
	}
	if err := runner.recoverSandbox(context.Background(), "owned-name", metadata); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []string{"owned"}) {
		t.Fatalf("recovery crossed ownership boundary: %v", deleted)
	}
}

func TestRecoveryReportsProviderErrors(t *testing.T) {
	runner := Runner{Config: newMockRunnerConfig("http://example.test", "")}
	metadata := runner.creationMetadata("run-owned")
	providerErr := errors.New("provider unavailable")
	runner.Recovery = mockRecovery{
		list: func(context.Context, map[string]string) ([]canaryapi.Sandbox, error) { return nil, providerErr },
	}
	if err := runner.recoverSandbox(context.Background(), "owned-name", metadata); !errors.Is(err, providerErr) {
		t.Fatalf("list failure hidden: %v", err)
	}
	runner.Recovery = mockRecovery{
		list: func(context.Context, map[string]string) ([]canaryapi.Sandbox, error) {
			return []canaryapi.Sandbox{{ID: "owned", Name: "owned-name", Metadata: metadata}}, nil
		},
		delete: func(context.Context, string) error { return providerErr },
	}
	if err := runner.recoverSandbox(context.Background(), "owned-name", metadata); !errors.Is(err, providerErr) {
		t.Fatalf("delete failure hidden: %v", err)
	}
}
