package canaryapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGetSandboxNotFoundIncludesMethodAndPath(t *testing.T) {
	t.Parallel()

	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			return nil, fmt.Errorf("unexpected method %s", req.Method)
		}
		if req.URL.Path != "/sandboxes/abc" {
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("missing")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	client := NewClient(httpClient, "https://api.example", "api-key", "preview.example")

	_, err := client.GetSandbox(context.Background(), "abc")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "GET /sandboxes/abc") {
		t.Fatalf("expected method and path in error, got %q", got)
	}
}

func TestUnexpectedStatusIncludesMethodAndPath(t *testing.T) {
	t.Parallel()

	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return nil, fmt.Errorf("unexpected method %s", req.Method)
		}
		if req.URL.Path != "/sandboxes" {
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader("boom")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	client := NewClient(httpClient, "https://api.example", "api-key", "preview.example")

	_, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{Name: "test"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !strings.Contains(got, "POST /sandboxes") {
		t.Fatalf("expected method and path in error, got %q", got)
	}
	if !strings.Contains(err.Error(), "unexpected status 500") {
		t.Fatalf("expected status in error, got %q", err.Error())
	}
}

func TestWriteFileUsesSandboxTokenAndPathQuery(t *testing.T) {
	t.Parallel()

	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != "preview.example" {
			return nil, fmt.Errorf("unexpected URL %s", req.URL)
		}
		if req.Method != http.MethodPost {
			return nil, fmt.Errorf("unexpected method %s", req.Method)
		}
		if req.URL.Path != "/files" {
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
		if got := req.URL.Query().Get("path"); got != "/tmp/verification-utilities/verify_disk.sh" {
			return nil, fmt.Errorf("unexpected query path %q", got)
		}
		if got := req.Header.Get("X-Access-Token"); got != "sandbox-token" {
			return nil, fmt.Errorf("unexpected access token %q", got)
		}
		if got := req.Header.Get("X-Superserve-Sandbox-Id"); got != "sb-123" {
			return nil, fmt.Errorf("unexpected sandbox id %q", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/octet-stream" {
			return nil, fmt.Errorf("unexpected content type %q", got)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if string(body) != "echo hi" {
			return nil, fmt.Errorf("unexpected body %q", string(body))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"path":"/tmp/verification-utilities/verify_disk.sh"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	client := NewClient(httpClient, "https://api.example", "api-key", "preview.example")

	if err := client.WriteFile(context.Background(), "sb-123", "sandbox-token", "/tmp/verification-utilities/verify_disk.sh", []byte("echo hi")); err != nil {
		t.Fatalf("WriteFile returned %v", err)
	}
}

func TestPublishPreviewPortUsesPreviewPortEndpoint(t *testing.T) {
	t.Parallel()

	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return nil, fmt.Errorf("unexpected method %s", req.Method)
		}
		if req.URL.Path != "/sandboxes/sb-123/preview-ports" {
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
		if got := req.Header.Get("X-API-Key"); got != "api-key" {
			return nil, fmt.Errorf("unexpected api key %q", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			return nil, fmt.Errorf("unexpected content type %q", got)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if string(body) != `{"port":18080,"access":"public"}` {
			return nil, fmt.Errorf("unexpected body %q", string(body))
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	client := NewClient(httpClient, "https://api.example", "api-key", "preview.example")

	if err := client.PublishPreviewPort(context.Background(), "sb-123", PublishPreviewPortRequest{Port: 18080, Access: "public"}); err != nil {
		t.Fatalf("PublishPreviewPort returned %v", err)
	}
}

func TestPublishPreviewPortRejectsUnexpectedStatus(t *testing.T) {
	t.Parallel()

	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader("boom")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	client := NewClient(httpClient, "https://api.example", "api-key", "preview.example")

	err := client.PublishPreviewPort(context.Background(), "sb-123", PublishPreviewPortRequest{Port: 18080, Access: "public"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !strings.Contains(got, "unexpected status 500") {
		t.Fatalf("unexpected error %q", got)
	}
}
