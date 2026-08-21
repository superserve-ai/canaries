package uicanary

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
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

func setupMockConsoleServer() *httptest.Server {
	mux := http.NewServeMux()

	var stateStatus = "Active"

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
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Sign In</title></head>
<body>
  <h1>Sign In</h1>
  <form method="POST" action="/auth/signin">
    <input type="email" placeholder="Email" name="email" value="" />
    <input type="password" placeholder="Password" name="password" value="" />
    <button type="submit">Sign In</button>
  </form>
</body>
</html>`)
	})

	mux.HandleFunc("/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Sandboxes</title></head>
<body>
  <h1>Sandboxes</h1>
  <button id="create-btn" onclick="document.getElementById('dialog').style.display='block'">Create sandbox</button>

  <div id="dialog" style="display:none;">
    <input type="text" placeholder="my-sandbox" id="name-input" />
    <button id="submit-create" onclick="window.location.href='/sandboxes/sb-mock-123/'">Create Sandbox</button>
  </div>
</body>
</html>`)
	})

	mux.HandleFunc("/sandboxes/sb-mock-123/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Sandbox Detail</title></head>
<body>
  <section id="hero">
    <h1>ui-canary-test</h1>
    <span id="status-badge">%s</span>
  </section>

  <button id="stop-btn" onclick="document.getElementById('status-badge').innerText='Paused'">Stop</button>
  <button id="start-btn" onclick="document.getElementById('status-badge').innerText='Active'">Start</button>

  <button aria-label="More actions" onclick="document.getElementById('menu').style.display='block'">More actions</button>
  <div id="menu" style="display:none;">
    <div role="menuitem" onclick="document.getElementById('delete-dialog').style.display='block'">Delete sandbox</div>
  </div>

  <div id="delete-dialog" style="display:none;">
    <input placeholder="ui-canary-mock" id="delete-input" />
    <button id="confirm-del" onclick="window.location.href='/sandboxes/'">Delete</button>
  </div>
</body>
</html>`, stateStatus)
	})

	mux.HandleFunc("/sandboxes/sb-mock-123/terminal/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Terminal</title></head>
<body>
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
        var lines = document.querySelector('.xterm-rows');
        var div = document.createElement('div');
        div.innerText = ta.value;
        lines.appendChild(div);
      }
    });
  </script>
</body>
</html>`)
	})

	return httptest.NewServer(mux)
}

func TestUIRunnerWithMockServer(t *testing.T) {
	server := setupMockConsoleServer()
	defer server.Close()

	artifactsDir, err := os.MkdirTemp("", "ui-canary-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(artifactsDir)

	baseCfg := config.Config{
		Environment: "staging",
		Region:      "us-central1",
		Target:      "staging-us-central1",
		RunTimeout:  30 * time.Second,
	}

	cfg := Config{
		BaseConfig:      baseCfg,
		ConsoleURL:      server.URL,
		Email:           "canary@superserve.ai",
		Password:        "password123",
		Headless:        true,
		ArtifactsDir:    artifactsDir,
		StepTimeout:     5 * time.Second,
		TerminalTimeout: 5 * time.Second,
	}

	runner := Runner{
		Config:  cfg,
		Locker:  lock.NoopLock{},
		Metrics: metrics.NoopProvider{},
		Clock:   time.Now,
	}

	// This integration test runs if playwright browser is available
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	err = runner.Run(ctx)
	if err != nil {
		t.Fatalf("Runner failed with email/password auth: %v", err)
	}
}

func TestAuthenticateInvalidCredentials(t *testing.T) {
	server := setupMockConsoleServer()
	defer server.Close()

	baseCfg := config.Config{
		Environment: "staging",
		Region:      "us-central1",
		Target:      "staging-us-central1",
		RunTimeout:  30 * time.Second,
	}

	cfg := Config{
		BaseConfig:      baseCfg,
		ConsoleURL:      server.URL,
		Email:           "", // invalid empty credentials
		Password:        "",
		Headless:        true,
		StepTimeout:     3 * time.Second,
		TerminalTimeout: 3 * time.Second,
	}

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
