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
