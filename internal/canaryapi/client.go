package canaryapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	httpClient    *http.Client
	baseURL       string
	apiKey        string
	previewDomain string
}

func NewClient(httpClient *http.Client, baseURL, apiKey, previewDomain string) *Client {
	return &Client{
		httpClient:    httpClient,
		baseURL:       strings.TrimRight(baseURL, "/"),
		apiKey:        apiKey,
		previewDomain: previewDomain,
	}
}

type Sandbox struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	AccessToken      string            `json:"access_token"`
	Metadata         map[string]string `json:"metadata"`
	AutoDeleteAt     *time.Time        `json:"auto_delete_at"`
	TimeoutSeconds   *int              `json:"timeout_seconds,omitempty"`
	AutoDeleteSecond *int              `json:"auto_delete_seconds,omitempty"`
}

type CreateSandboxRequest struct {
	Name              string            `json:"name"`
	FromTemplate      string            `json:"from_template,omitempty"`
	TimeoutSeconds    int               `json:"timeout_seconds,omitempty"`
	AutoDeleteSeconds int               `json:"auto_delete_seconds,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

type UpdateSandboxRequest struct {
	Metadata          map[string]string `json:"metadata,omitempty"`
	AutoDeleteSeconds *int              `json:"auto_delete_seconds,omitempty"`
	TimeoutSeconds    *int              `json:"timeout_seconds,omitempty"`
}

type PublishPreviewPortRequest struct {
	Port   int    `json:"port"`
	Access string `json:"access"`
}

type ExecRequest struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir,omitempty"`
	TimeoutS   int    `json:"timeout_s,omitempty"`
}

type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type ResumeResponse struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	AccessToken string `json:"access_token"`
}

type ErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodPost, "/sandboxes", req, &out)
	return out, err
}

func (c *Client) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	var out Sandbox
	err := c.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *Client) ListSandboxes(ctx context.Context, query map[string]string) ([]Sandbox, error) {
	u, _ := url.Parse(c.baseURL + "/sandboxes")
	values := u.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	u.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := requireStatus(http.MethodGet, u.Path, resp, http.StatusOK); err != nil {
		return nil, err
	}
	var out []Sandbox
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) PauseSandbox(ctx context.Context, id string) error {
	return c.doNoContent(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/pause")
}

func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	return c.doNoContent(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id))
}

func (c *Client) ResumeSandbox(ctx context.Context, id string) (ResumeResponse, error) {
	var out ResumeResponse
	err := c.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/resume", map[string]any{}, &out)
	return out, err
}

func (c *Client) UpdateSandbox(ctx context.Context, id string, req UpdateSandboxRequest) error {
	return c.doPatch(ctx, "/sandboxes/"+url.PathEscape(id), req)
}

func (c *Client) PublishPreviewPort(ctx context.Context, sandboxID string, req PublishPreviewPortRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sandboxes/"+url.PathEscape(sandboxID)+"/preview-ports", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("X-API-Key", c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return require2xx(http.MethodPost, "/sandboxes/"+url.PathEscape(sandboxID)+"/preview-ports", resp)
}

func (c *Client) Exec(ctx context.Context, sandboxID, accessToken string, req ExecRequest) (ExecResult, error) {
	var out ExecResult
	payload, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	target := "https://" + c.previewDomain + "/exec"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Access-Token", accessToken)
	httpReq.Header.Set("X-Superserve-Sandbox-Id", sandboxID)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := requireStatus(http.MethodPost, "/exec", resp, http.StatusOK); err != nil {
		return out, err
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) WriteFile(ctx context.Context, sandboxID, accessToken, path string, content []byte) error {
	target, err := url.Parse("https://" + c.previewDomain + "/files")
	if err != nil {
		return err
	}
	values := target.Query()
	values.Set("path", path)
	target.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Access-Token", accessToken)
	req.Header.Set("X-Superserve-Sandbox-Id", sandboxID)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := requireStatus(http.MethodPost, "/files", resp, http.StatusOK, http.StatusCreated); err != nil {
		return err
	}
	return nil
}

func (c *Client) PreviewURL(sandboxID string, port int) string {
	return fmt.Sprintf("https://%d-%s.%s", port, sandboxID, c.previewDomain)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return requireStatus(method, path, resp, http.StatusNoContent)
	}
	switch method {
	case http.MethodPost:
		if strings.HasSuffix(path, "/resume") {
			if err := requireStatus(method, path, resp, http.StatusOK); err != nil {
				return err
			}
		} else if path == "/sandboxes" {
			if err := requireStatus(method, path, resp, http.StatusCreated); err != nil {
				return err
			}
		} else if err := requireStatus(method, path, resp, http.StatusOK, http.StatusCreated); err != nil {
			return err
		}
	default:
		if err := requireStatus(method, path, resp, http.StatusOK); err != nil {
			return err
		}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) doNoContent(ctx context.Context, method, path string) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return requireStatus(method, path, resp, http.StatusNoContent)
}

func (c *Client) doPatch(ctx context.Context, path string, in any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return requireStatus(http.MethodPatch, path, resp, http.StatusNoContent, http.StatusOK)
}

func requireStatus(method, path string, resp *http.Response, allowed ...int) error {
	for _, status := range allowed {
		if resp.StatusCode == status {
			return nil
		}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	return fmt.Errorf("%s %s: unexpected status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
}

func require2xx(method, path string, resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return requireStatus(method, path, resp)
}

var ErrNotFound = errors.New("resource not found")
