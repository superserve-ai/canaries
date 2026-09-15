package uicanary

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/playwright-community/playwright-go"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

func TestExtractSandboxIDFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://console.superserve.ai/sandboxes/sb-12345/terminal/", "sb-12345"},
		{"https://console.superserve.ai/sandboxes/sb-abcdef?tab=settings", "sb-abcdef"},
		{"https://console.superserve.ai/sandboxes/sb-999/", "sb-999"},
		{"http://localhost:3000/sandboxes/sb-local", "sb-local"},
	}

	for _, tt := range tests {
		got := extractSandboxIDFromURL(tt.url)
		if got != tt.want {
			t.Errorf("extractSandboxIDFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

type mockServerState struct {
	sync.Mutex
	receivedBypassHeader       string
	receivedBypassCookieHeader string
	deletedSandbox             bool
	failTerminal               bool
	terminalCommandExecuted    bool
	pausedSandbox              bool
	resumedSandbox             bool
	rejectPause                bool
	rejectResume               bool
	rejectDelete               bool
	rejectCreation             bool
	initialStatus              string
	externalURL                string
	externalReceivedBypass     string
	externalReceivedCookie     string
	externalHitCount           int
}

func setupMockConsoleServer(opts ...*mockServerState) *httptest.Server {
	var state *mockServerState
	if len(opts) > 0 {
		state = opts[0]
	}

	mux := http.NewServeMux()
	var stateStatus = "Active"
	if state != nil {
		state.Lock()
		if state.initialStatus != "" {
			stateStatus = state.initialStatus
		}
		state.Unlock()
	}

	mux.HandleFunc("/auth/signin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			email := r.FormValue("email")
			password := r.FormValue("password")
			if email == "" || password == "" {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprintf(w, `<!DOCTYPE html><html><body><p role="alert" class="text-destructive">Invalid credentials</p></body></html>`)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:  "sb-auth-token.0",
				Value: "valid-session",
				Path:  "/",
			})
			http.Redirect(w, r, "/sandboxes/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		externalHTML := ""
		if state != nil {
			state.Lock()
			if state.externalURL != "" {
				externalHTML = fmt.Sprintf(`<img src="%s/external-beacon" />`, state.externalURL)
			}
			state.Unlock()
		}
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Sign In</title></head>
<body>
  <h1>Sign In</h1>
  %s
  <form method="POST" action="/auth/signin">
    <input type="email" placeholder="Email" name="email" value="" />
    <input type="password" placeholder="Password" name="password" value="" />
    <button type="submit">Sign In</button>
  </form>
</body>
</html>`, externalHTML)
	})

	mux.HandleFunc("/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("deleted") == "true" {
			if state != nil {
				state.Lock()
				state.deletedSandbox = true
				state.Unlock()
			}
		}
		w.Header().Set("Content-Type", "text/html")
		reject := false
		if state != nil {
			state.Lock()
			reject = state.rejectCreation
			state.Unlock()
		}

		submitAction := `document.getElementById('dialog').style.display='none'; document.getElementById('connect-dialog').style.display='block';`
		if reject {
			submitAction = `fetch('/api/sandboxes', {method: 'POST'}).catch(function(){}); document.getElementById('create-err-alert').style.display='block';`
		}

		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Sandboxes</title></head>
<body>
  <h1>Sandboxes</h1>
  <button id="create-btn" onclick="document.getElementById('dialog').style.display='block'">Create sandbox</button>
  <table id="sandboxes-table"><tbody><tr><td>Ready</td></tr></tbody></table>

  <div id="dialog" role="dialog" style="display:none;">
    <input type="text" placeholder="my-sandbox" id="name-input" />
    <div id="create-err-alert" role="alert" class="text-destructive" style="display:none;">Account quota exceeded. Upgrade required.</div>
    <button id="submit-create" onclick="%s">Create Sandbox</button>
  </div>

  <div id="connect-dialog" role="dialog" style="display:none;">
    <h3>Connect to Sandbox</h3>
    <pre>const sandbox = await Sandbox.connect("sb-mock-123", {
  apiKey: process.env.SUPERSERVE_API_KEY,
});</pre>
    <button onclick="window.location.href='/sandboxes/sb-mock-123/terminal/'">Open Terminal</button>
    <button onclick="document.getElementById('connect-dialog').style.display='none'">Done</button>
  </div>
</body>
</html>`, submitAction)
	})

	mux.HandleFunc("/api/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprintln(w, `{"message":"Account quota exceeded. Upgrade required."}`)
	})

	mux.HandleFunc("/sandboxes/sb-mock-123/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		rejectPause := false
		rejectResume := false
		rejectDelete := false
		if state != nil {
			state.Lock()
			rejectPause = state.rejectPause
			rejectResume = state.rejectResume
			rejectDelete = state.rejectDelete
			state.Unlock()
		}

		stopAction := `fetch('/pause-sandbox', {method: 'POST'}).catch(function(){}); document.getElementById('status-badge').innerText='Paused';`
		if rejectPause {
			stopAction = `fetch('/pause-sandbox', {method: 'POST'}).catch(function(){}); document.getElementById('pause-err-toast').style.display='block';`
		}

		startAction := `fetch('/resume-sandbox', {method: 'POST'}).catch(function(){}); document.getElementById('status-badge').innerText='Active';`
		if rejectResume {
			startAction = `fetch('/resume-sandbox', {method: 'POST'}).catch(function(){}); document.getElementById('resume-err-toast').style.display='block';`
		}

		deleteAction := `window.location.href='/sandboxes/?deleted=true'`
		if rejectDelete {
			deleteAction = `document.getElementById('delete-err-toast').style.display='block';`
		}

		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Sandbox Detail</title></head>
<body>
  <section id="hero">
    <h1>ui-canary-test</h1>
    <span id="status-badge">%s</span>
  </section>

  <div id="pause-err-toast" data-sonner-toast="" data-type="error" style="display:none;">Failed to pause sandbox: operation rejected</div>
  <div id="resume-err-toast" data-sonner-toast="" data-type="error" style="display:none;">Failed to resume sandbox: operation rejected</div>
  <div id="delete-err-toast" data-sonner-toast="" data-type="error" style="display:none;">delete sandbox rejected: Failed to delete sandbox: operation rejected</div>
  <button id="stop-btn" onclick="%s">Stop</button>
  <button id="start-btn" onclick="%s">Start</button>

  <button aria-label="More actions" onclick="document.getElementById('menu').style.display='block'">More actions</button>
  <div id="menu" style="display:none;">
    <div role="menuitem" onclick="document.getElementById('delete-dialog').style.display='block'">Delete sandbox</div>
  </div>

  <div id="delete-dialog" role="dialog" style="display:none;">
    <input placeholder="ui-canary-mock" id="delete-input" />
    <button id="confirm-del" onclick="%s">Delete</button>
  </div>
</body>
</html>`, stateStatus, stopAction, startAction, deleteAction)
	})

	mux.HandleFunc("/pause-sandbox", func(w http.ResponseWriter, r *http.Request) {
		if state != nil {
			state.Lock()
			state.pausedSandbox = true
			state.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/resume-sandbox", func(w http.ResponseWriter, r *http.Request) {
		if state != nil {
			state.Lock()
			state.resumedSandbox = true
			state.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/terminal-command", func(w http.ResponseWriter, r *http.Request) {
		if state != nil {
			state.Lock()
			state.terminalCommandExecuted = true
			state.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/sandboxes/sb-mock-123/terminal/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		shouldFail := false
		if state != nil {
			state.Lock()
			shouldFail = state.failTerminal
			state.Unlock()
		}

		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Terminal</title></head>
<body>
  <div class="header">
    <span id="status-badge">%s</span>
    <a href="/sandboxes/sb-mock-123/">ui-canary-test</a>
  </div>
  <div class="xterm" style="position:relative; width:100vw; height:100vh;" onclick="document.querySelector('.xterm-helper-textarea').focus()">
    <textarea class="xterm-helper-textarea" style="opacity:0; position:absolute; top:0; left:0;"></textarea>
    <div class="xterm-rows">
      <div id="term-line">root@sandbox-mock:~# </div>
    </div>
  </div>
  <script>
    var ta = document.querySelector('.xterm-helper-textarea');
    ta.addEventListener('input', function(e) {
      document.getElementById('term-line').innerText = 'root@sandbox-mock:~# ' + e.target.value;
    });
    window.addEventListener('keydown', function(e) {
      if (e.key === 'Enter') {
        fetch('/terminal-command', {method: 'POST'}).catch(function(){});
        var lines = document.querySelector('.xterm-rows');
        var div = document.createElement('div');
        var val = ta.value;
        var match = val.match(/\$\(\(\s*(\d+)\s*\+\s*(\d+)\s*\)\)/);
        if (match) {
          if (%t) {
            div.innerText = 'terminal_error';
          } else {
            var sum = parseInt(match[1], 10) + parseInt(match[2], 10);
            div.innerText = 'RES_UI_' + sum;
          }
        } else {
          div.innerText = val;
        }
        lines.appendChild(div);
      }
    });
  </script>
</body>
</html>`, stateStatus, shouldFail)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if state != nil {
			state.Lock()
			if v := r.Header.Get("x-vercel-protection-bypass"); v != "" {
				state.receivedBypassHeader = v
			}
			if v := r.Header.Get("x-vercel-set-bypass-cookie"); v != "" {
				state.receivedBypassCookieHeader = v
			}
			state.Unlock()
		}
		mux.ServeHTTP(w, r)
	})

	return httptest.NewServer(handler)
}

func skipIfPlaywrightUnavailable(t *testing.T) {
	t.Helper()
	pw, err := playwright.Run()
	if err != nil {
		t.Skipf("skipping UI canary test: playwright driver unavailable: %v", err)
		return
	}
	defer pw.Stop()
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(true)})
	if err != nil {
		t.Skipf("skipping UI canary test: chromium browser unavailable: %v", err)
		return
	}
	_ = browser.Close()
}

func newMockRunnerConfig(serverURL, bypassToken string) Config {
	return Config{
		BaseConfig: config.Config{
			Environment: "staging",
			Region:      "us-central1",
			Target:      "staging-us-central1",
			RunTimeout:  30 * time.Second,
		},
		ConsoleURL:             serverURL,
		Email:                  "canary@superserve.ai",
		Password:               "password123",
		VercelProtectionBypass: bypassToken,
		Headless:               true,
		StepTimeout:            5 * time.Second,
		TerminalTimeout:        5 * time.Second,
	}
}

func TestUIRunnerWithMockServer(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	state := &mockServerState{}
	server := setupMockConsoleServer(state)
	defer server.Close()

	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.Lock()
		state.externalHitCount++
		state.externalReceivedBypass = r.Header.Get("x-vercel-protection-bypass")
		state.externalReceivedCookie = r.Header.Get("x-vercel-set-bypass-cookie")
		state.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer externalServer.Close()

	state.Lock()
	state.externalURL = externalServer.URL
	state.Unlock()

	artifactsDir, err := os.MkdirTemp("", "ui-canary-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(artifactsDir)

	cfg := newMockRunnerConfig(server.URL, "test-bypass-secret-123")
	cfg.ArtifactsDir = artifactsDir

	var taggedSandboxID string
	var taggedMetadata map[string]string
	mockTagger := &mockSandboxTagger{
		tagFn: func(ctx context.Context, sandboxID string, metadata map[string]string) error {
			taggedSandboxID = sandboxID
			taggedMetadata = metadata
			return nil
		},
	}

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
		Tagger:  mockTagger,
	}

	// This integration test runs if playwright browser is available
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	err = runner.Run(ctx)
	if err != nil {
		t.Fatalf("Runner failed with email/password auth: %v", err)
	}

	if taggedSandboxID != "sb-mock-123" {
		t.Errorf("expected taggedSandboxID 'sb-mock-123', got %q", taggedSandboxID)
	}
	if taggedMetadata[sandboxmetadata.KeyManagedBy] != sandboxmetadata.ManagedByCanaryLegacy {
		t.Errorf("expected managed_by %q, got %q", sandboxmetadata.ManagedByCanaryLegacy, taggedMetadata[sandboxmetadata.KeyManagedBy])
	}

	state.Lock()
	defer state.Unlock()
	if state.receivedBypassHeader != "test-bypass-secret-123" {
		t.Errorf("expected bypass header 'test-bypass-secret-123', got %q", state.receivedBypassHeader)
	}
	if state.receivedBypassCookieHeader != "true" {
		t.Errorf("expected bypass cookie header 'true', got %q", state.receivedBypassCookieHeader)
	}
	if !state.deletedSandbox {
		t.Errorf("expected sandbox to be deleted in UI lifecycle")
	}
	if state.externalHitCount == 0 {
		t.Errorf("expected external server to be requested by browser")
	}
	if state.externalReceivedBypass != "" {
		t.Errorf("expected cross-origin request to have NO bypass header, got %q", state.externalReceivedBypass)
	}
	if state.externalReceivedCookie != "" {
		t.Errorf("expected cross-origin request to have NO bypass cookie header, got %q", state.externalReceivedCookie)
	}
}

type mockSandboxTagger struct {
	tagFn func(ctx context.Context, sandboxID string, metadata map[string]string) error
}

func (m *mockSandboxTagger) TagSandbox(ctx context.Context, sandboxID string, metadata map[string]string) error {
	if m.tagFn != nil {
		return m.tagFn(ctx, sandboxID, metadata)
	}
	return nil
}

func TestUIRunnerWithoutBypassHeaders(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	state := &mockServerState{}
	server := setupMockConsoleServer(state)
	defer server.Close()

	cfg := newMockRunnerConfig(server.URL, "") // unconfigured bypass

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Runner failed: %v", err)
	}

	state.Lock()
	defer state.Unlock()
	if state.receivedBypassHeader != "" {
		t.Errorf("expected no bypass header, got %q", state.receivedBypassHeader)
	}
	if state.receivedBypassCookieHeader != "" {
		t.Errorf("expected no bypass cookie header, got %q", state.receivedBypassCookieHeader)
	}
}

func TestDeferredCleanupOnPostCreateFailure(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	state := &mockServerState{failTerminal: true}
	server := setupMockConsoleServer(state)
	defer server.Close()

	cfg := newMockRunnerConfig(server.URL, "")
	cfg.StepTimeout = 3 * time.Second
	cfg.TerminalTimeout = 1 * time.Second

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := runner.Run(ctx)
	if err == nil {
		t.Fatal("expected runner to fail on terminal step")
	}

	state.Lock()
	defer state.Unlock()
	if !state.deletedSandbox {
		t.Errorf("expected deferred cleanup to delete sandbox when terminal step failed")
	}
}

func TestAuthenticateInvalidCredentials(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	server := setupMockConsoleServer()
	defer server.Close()

	cfg := newMockRunnerConfig(server.URL, "")
	cfg.Email = "" // invalid empty credentials
	cfg.Password = ""
	cfg.StepTimeout = 3 * time.Second
	cfg.TerminalTimeout = 3 * time.Second

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := runner.Run(ctx)
	if err == nil {
		t.Fatal("expected runner to fail with invalid credentials")
	}
}

func TestGenerateTerminalCommand(t *testing.T) {
	for i := 0; i < 20; i++ {
		cmd, expected := generateTerminalCommand()

		// Verify command structure: echo "RES_UI_$((nonceA + nonceB))"
		if !strings.HasPrefix(cmd, `echo "RES_UI_$((`) || !strings.HasSuffix(cmd, `))"`) {
			t.Fatalf("unexpected command format: %q", cmd)
		}

		// Verify expected output format: RES_UI_<sum>
		if !strings.HasPrefix(expected, "RES_UI_") {
			t.Fatalf("unexpected expectedOutput format: %q", expected)
		}

		// Extract nonces
		var a, b int
		n, err := fmt.Sscanf(cmd, `echo "RES_UI_$((%d + %d))"`, &a, &b)
		if err != nil || n != 2 {
			t.Fatalf("failed to parse nonces from command %q: %v", cmd, err)
		}

		if a < 1000 || a >= 10000 || b < 1000 || b >= 10000 {
			t.Errorf("nonces out of range [1000, 9999]: a=%d, b=%d", a, b)
		}
		if a == b {
			t.Errorf("expected distinct nonces, got a=%d == b=%d", a, b)
		}

		wantExpected := fmt.Sprintf("RES_UI_%d", a+b)
		if expected != wantExpected {
			t.Errorf("expectedOutput = %q, want %q", expected, wantExpected)
		}
	}
}

func TestConcurrentIDDiscoveryRace(t *testing.T) {
	const goroutines = 50
	var discoveredCount int32
	var firstDiscoveredID string
	var mu sync.Mutex

	tracker := newSandboxIDTracker(func(id string) {
		atomic.AddInt32(&discoveredCount, 1)
		mu.Lock()
		firstDiscoveredID = id
		mu.Unlock()
	})

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			idCandidate := fmt.Sprintf("sb-test-%d", idx%5)
			// Concurrently set and read
			_ = tracker.Set(idCandidate)
			_ = tracker.Get()
		}(i)
	}
	wg.Wait()

	finalID := tracker.Get()
	if finalID == "" {
		t.Fatal("expected non-empty final ID")
	}
	if count := atomic.LoadInt32(&discoveredCount); count != 1 {
		t.Fatalf("expected onDiscovered callback to fire exactly once, fired %d times", count)
	}
	mu.Lock()
	defer mu.Unlock()
	if firstDiscoveredID != finalID {
		t.Fatalf("expected discovered ID %q to match final ID %q", firstDiscoveredID, finalID)
	}
}

func TestCreationRejectionFailFast(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	state := &mockServerState{rejectCreation: true}
	server := setupMockConsoleServer(state)
	defer server.Close()

	cfg := newMockRunnerConfig(server.URL, "")
	cfg.StepTimeout = 15 * time.Second // High timeout; fail-fast should abort in < 6s

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	err := runner.Run(ctx)
	duration := time.Since(start)

	if err == nil {
		t.Fatal("expected runner to fail on rejected creation")
	}

	if !strings.Contains(err.Error(), "Account quota exceeded") && !strings.Contains(err.Error(), "rejected") {
		t.Errorf("expected error message to contain causal rejection info, got: %v", err)
	}

	if duration > 12*time.Second {
		t.Errorf("expected fail-fast abort well before 15s timeout, took %v", duration)
	}
}

func TestFailClosedTaggingRetryAndCleanup(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	state := &mockServerState{}
	server := setupMockConsoleServer(state)
	defer server.Close()

	cfg := newMockRunnerConfig(server.URL, "")
	cfg.StepTimeout = 5 * time.Second

	var tagAttempts int32
	mockTagger := &mockSandboxTagger{
		tagFn: func(ctx context.Context, sandboxID string, metadata map[string]string) error {
			atomic.AddInt32(&tagAttempts, 1)
			return fmt.Errorf("simulated metadata tagging 500 error")
		},
	}

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
		Tagger:  mockTagger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	err := runner.Run(ctx)
	if err == nil {
		t.Fatal("expected runner to fail when tagging fails")
	}

	if attempts := atomic.LoadInt32(&tagAttempts); attempts != 3 {
		t.Errorf("expected exactly 3 tagging attempts, got %d", attempts)
	}

	if !strings.Contains(err.Error(), "tag sandbox after 3 attempts") {
		t.Errorf("expected error to cite tagging failure after 3 attempts, got: %v", err)
	}

	state.Lock()
	deleted := state.deletedSandbox
	terminalHit := state.terminalCommandExecuted
	pausedHit := state.pausedSandbox
	resumedHit := state.resumedSandbox
	state.Unlock()

	if !deleted {
		t.Errorf("expected unowned sandbox to be deleted synchronously immediately after tagging failure")
	}
	if terminalHit {
		t.Errorf("expected terminal execution to NEVER be invoked after tagging failure")
	}
	if pausedHit {
		t.Errorf("expected pause step to NEVER be invoked after tagging failure")
	}
	if resumedHit {
		t.Errorf("expected resume step to NEVER be invoked after tagging failure")
	}
}

func TestPauseRejectionFailFast(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	state := &mockServerState{rejectPause: true}
	server := setupMockConsoleServer(state)
	defer server.Close()

	cfg := newMockRunnerConfig(server.URL, "")
	cfg.StepTimeout = 10 * time.Second

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	err := runner.Run(ctx)
	if err == nil {
		t.Fatal("expected runner to fail on pause rejection toast")
	}

	if !strings.Contains(err.Error(), "Failed to pause sandbox") && !strings.Contains(err.Error(), "operation rejected") {
		t.Errorf("expected error message to contain pause rejection causal info, got: %v", err)
	}
}

func TestResumeRejectionFailFast(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	state := &mockServerState{rejectResume: true}
	server := setupMockConsoleServer(state)
	defer server.Close()

	cfg := newMockRunnerConfig(server.URL, "")
	cfg.StepTimeout = 10 * time.Second

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	err := runner.Run(ctx)
	if err == nil {
		t.Fatal("expected runner to fail on resume rejection toast")
	}

	if !strings.Contains(err.Error(), "Failed to resume sandbox") && !strings.Contains(err.Error(), "operation rejected") {
		t.Errorf("expected error message to contain resume rejection causal info, got: %v", err)
	}
}

func TestDeleteRejectionFailFast(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	state := &mockServerState{rejectDelete: true}
	server := setupMockConsoleServer(state)
	defer server.Close()

	cfg := newMockRunnerConfig(server.URL, "")
	cfg.StepTimeout = 10 * time.Second

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	err := runner.Run(ctx)
	if err == nil {
		t.Fatal("expected runner to fail on delete rejection toast")
	}

	if !strings.Contains(err.Error(), "delete sandbox rejected") && !strings.Contains(err.Error(), "Failed to delete sandbox") {
		t.Errorf("expected error message to contain delete rejection causal info, got: %v", err)
	}
}

type recordLocker struct {
	mu          sync.Mutex
	acquiredKey string
	ttl         time.Duration
}

func (l *recordLocker) Acquire(_ context.Context, key string, ttl time.Duration) (lock.Outcome, lock.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquiredKey = key
	l.ttl = ttl
	return lock.OutcomeAlreadyRunning, nil, nil
}

func TestUILockKeyIsolation(t *testing.T) {
	locker := &recordLocker{}
	cfg := Config{
		BaseConfig: config.Config{
			Target: "staging-us-central1",
		},
	}
	runner := Runner{
		Config: cfg,
		Locker: locker,
	}

	err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("expected nil error on OutcomeAlreadyRunning, got: %v", err)
	}

	locker.mu.Lock()
	gotKey := locker.acquiredKey
	locker.mu.Unlock()

	wantKey := "staging-us-central1-ui"
	if gotKey != wantKey {
		t.Fatalf("acquired lock key = %q, want %q", gotKey, wantKey)
	}
}

func TestAuthenticateDelayedRedirect(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	var (
		mu                 sync.Mutex
		clientRedirectDone bool
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/signin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "text/html")
			// Delayed JS redirect at 4s.
			// Prior to the fix, the polling loop called page.Goto after 3s,
			// which aborted in-flight redirects. With the fix, client redirect succeeds.
			fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Authenticating</title></head>
<body>
  <p>Authenticating session...</p>
  <script>
    setTimeout(function() {
      window.location.href = '/sandboxes/?via=client-redirect';
    }, 4000);
  </script>
</body>
</html>`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Sign In</title></head>
<body>
  <form method="POST" action="/auth/signin">
    <input type="email" placeholder="Email" name="email" value="" />
    <input type="password" placeholder="Password" name="password" value="" />
    <button type="submit">Sign In</button>
  </form>
</body>
</html>`)
	})

	mux.HandleFunc("/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("via") == "client-redirect" {
			mu.Lock()
			clientRedirectDone = true
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html><html><body><h1>Sandboxes</h1></body></html>`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	pw, err := playwright.Run()
	if err != nil {
		t.Fatalf("playwright run: %v", err)
	}
	defer pw.Stop()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		t.Fatalf("chromium launch: %v", err)
	}
	defer browser.Close()

	page, err := browser.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()

	cfg := Config{
		ConsoleURL:  server.URL,
		Email:       "test@example.com",
		Password:    "password",
		StepTimeout: 10 * time.Second,
	}

	err = Authenticate(context.Background(), page, cfg)
	if err != nil {
		t.Fatalf("expected Authenticate to succeed with delayed redirect, got: %v", err)
	}

	mu.Lock()
	viaClient := clientRedirectDone
	mu.Unlock()
	if !viaClient {
		t.Fatalf("expected redirect to be initiated by client script (?via=client-redirect)")
	}
}

func TestResumeIgnoresTableActiveCell(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/sandboxes/sb-test-table/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Status hero displays 'Paused', but audit table has a cell with 'Active'.
		// The locator should ignore the table cell and not falsely report active.
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Sandbox Detail</title></head>
<body>
  <section id="hero">
    <h1>sandbox-table-test</h1>
    <span id="status-badge">Paused</span>
  </section>
  <button id="start-btn">Start</button>
  <div id="events">
    <table>
      <thead><tr><th>Event</th><th>Status</th></tr></thead>
      <tbody>
        <tr><td>Created</td><td>Active</td></tr>
      </tbody>
    </table>
  </div>
</body>
</html>`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	pw, err := playwright.Run()
	if err != nil {
		t.Fatalf("playwright run: %v", err)
	}
	defer pw.Stop()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		t.Fatalf("chromium launch: %v", err)
	}
	defer browser.Close()

	page, err := browser.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()

	runner := Runner{
		Config: Config{
			ConsoleURL: server.URL,
		},
		Clock: time.Now,
	}

	// 1 second timeout.
	// If locator matched td:has-text('Active'), resumeSandboxInUI would immediately return nil.
	err = runner.resumeSandboxInUI(page, "sb-test-table", 1000)
	if err == nil {
		t.Fatal("expected resumeSandboxInUI to time out ignoring td:has-text('Active'); got nil")
	}
	if !strings.Contains(err.Error(), "waiting for Active status after resume timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

type stepMetricsRecorder struct {
	metrics.NoopProvider
	mu    sync.Mutex
	steps []recordedStep
}

type recordedStep struct {
	step   string
	result string
}

func (r *stepMetricsRecorder) RecordStep(_ context.Context, _, _, _, _, step, result string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, recordedStep{step: step, result: result})
}

func TestCreateSandboxStepMetricReadinessFailure(t *testing.T) {
	skipIfPlaywrightUnavailable(t)

	// Set initialStatus to "Provisioning" so detail page never shows "Active"
	state := &mockServerState{initialStatus: "Provisioning"}
	server := setupMockConsoleServer(state)
	defer server.Close()

	recorder := &stepMetricsRecorder{}

	cfg := newMockRunnerConfig(server.URL, "")
	cfg.StepTimeout = 1500 * time.Millisecond

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: recorder,
		Clock:   time.Now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := runner.Run(ctx)
	if err == nil {
		t.Fatal("expected runner to fail on sandbox UI readiness timeout")
	}

	if !strings.Contains(err.Error(), "sandbox UI readiness") {
		t.Errorf("expected error to mention sandbox UI readiness, got: %v", err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	var createSandboxSuccessCount, createSandboxFailureCount int
	for _, s := range recorder.steps {
		if s.step == "create_sandbox" {
			if s.result == "success" {
				createSandboxSuccessCount++
			} else if s.result == "failure" {
				createSandboxFailureCount++
			}
		}
	}

	if createSandboxSuccessCount != 0 {
		t.Errorf("expected 0 create_sandbox success metrics, got %d", createSandboxSuccessCount)
	}
	if createSandboxFailureCount != 1 {
		t.Errorf("expected 1 create_sandbox failure metric, got %d", createSandboxFailureCount)
	}
}
