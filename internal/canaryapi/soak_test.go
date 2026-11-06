package canaryapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCreateSandboxSendsPrivatePreviewAndIdleTimeout(t *testing.T) {
	t.Parallel()
	idle := 60
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/sandboxes" || req.Header.Get("X-API-Key") != "api-key" {
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
		var got map[string]any
		if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
			return nil, err
		}
		if got["preview_access"] != "private" || got["idle_timeout_seconds"] != float64(60) || got["auto_delete_seconds"] != float64(25_200) {
			return nil, fmt.Errorf("unexpected create options: %v", got)
		}
		return soakResponse(req, http.StatusCreated, `{"id":"sb-123","preview_access":"private","idle_timeout_seconds":60}`), nil
	})}
	client := NewClient(httpClient, "https://api.example", "api-key", "preview.example")
	sb, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{
		Name: "soak", PreviewAccess: "private", IdleTimeoutSeconds: &idle, AutoDeleteSeconds: 25_200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.PreviewAccess != "private" || sb.IdleTimeoutSeconds == nil || *sb.IdleTimeoutSeconds != idle {
		t.Fatalf("unexpected sandbox response: %+v", sb)
	}
}

func TestActivateSandboxRefreshesConnectionCredentials(t *testing.T) {
	t.Parallel()
	calls := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		switch req.URL.Host + req.URL.Path {
		case "api.example/sandboxes/sb-123/activate":
			if req.Method != http.MethodPost || req.Header.Get("X-API-Key") != "api-key" {
				return nil, fmt.Errorf("unexpected activation request")
			}
			body, err := io.ReadAll(req.Body)
			if err != nil || string(body) != "{}" {
				return nil, fmt.Errorf("activation body = %q, error = %v", body, err)
			}
			return soakResponse(req, http.StatusOK, `{"id":"sb-123","status":"active","access_token":"fresh-token","routing_hint":"fresh-hint"}`), nil
		case "preview.example/exec":
			if req.Header.Get("X-Access-Token") != "fresh-token" || req.Header.Get("X-Superserve-Routing-Hint") != "fresh-hint" {
				return nil, fmt.Errorf("data-plane request did not use activated credentials")
			}
			return soakResponse(req, http.StatusOK, `{"stdout":"ok","exit_code":0}`), nil
		default:
			return nil, fmt.Errorf("unexpected request %s", req.URL)
		}
	})}
	client := NewClient(httpClient, "https://api.example", "api-key", "preview.example")
	client.rememberRoutingHint("sb-123", "old-hint")
	sb, err := client.ActivateSandbox(context.Background(), "sb-123")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != "active" || sb.AccessToken != "fresh-token" {
		t.Fatalf("unexpected activation result: %+v", sb)
	}
	if _, err := client.Exec(context.Background(), sb.ID, sb.AccessToken, ExecRequest{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("got %d requests, want activation and one execution", calls)
	}
}

func TestPrivatePreviewTokenAndDeleteContract(t *testing.T) {
	t.Parallel()
	calls := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("X-API-Key") != "api-key" {
			return nil, fmt.Errorf("missing API authentication")
		}
		switch req.Method + " " + req.URL.Path {
		case "POST /sandboxes/sb-123/preview-ports/18080/token":
			body, err := io.ReadAll(req.Body)
			if err != nil || string(body) != "{}" || req.Header.Get("Content-Type") != "application/json" {
				return nil, fmt.Errorf("unexpected token request body or content type")
			}
			return soakResponse(req, http.StatusOK, `{"token":"spv1.example.signature","port":18080,"header":"X-Superserve-Preview-Token","query_param":"superserve_preview_token","access":"private","preview_access":"private","token_version":2}`), nil
		case "DELETE /sandboxes/sb-123/preview-ports/18080":
			return soakResponse(req, http.StatusNoContent, ""), nil
		default:
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
	})}
	client := NewClient(httpClient, "https://api.example", "api-key", "preview.example")
	token, err := client.CreatePreviewToken(context.Background(), "sb-123", 18080)
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "spv1.example.signature" || token.Header != "X-Superserve-Preview-Token" || token.Port != 18080 || token.TokenVersion != 2 || token.Access != "private" {
		t.Fatalf("unexpected token response")
	}
	if err := client.DeletePreviewPort(context.Background(), "sb-123", 18080); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("got %d requests, want token mint and delete", calls)
	}
}

func TestSoakAPIReportsFailureWithoutRetry(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"activate", "token", "delete"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return soakResponse(req, http.StatusServiceUnavailable, `{"error":{"code":"sandbox_unavailable"}}`), nil
			})}
			client := NewClient(httpClient, "https://api.example", "api-key", "preview.example")
			var err error
			switch operation {
			case "activate":
				_, err = client.ActivateSandbox(context.Background(), "sb-123")
			case "token":
				_, err = client.CreatePreviewToken(context.Background(), "sb-123", 18080)
			case "delete":
				err = client.DeletePreviewPort(context.Background(), "sb-123", 18080)
			}
			var statusErr *HTTPStatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusServiceUnavailable || calls != 1 {
				t.Fatalf("error = %v, calls = %d; want 503 and one attempt", err, calls)
			}
		})
	}
}

func soakResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}
}
