package uicanary

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/playwright-community/playwright-go"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

// SandboxTagger is an optional dependency for tagging sandbox ownership metadata
// via the API immediately after the sandbox ID is recovered from the browser URL.
// This ensures the janitor can discover and reap sandboxes that leak if a run
// crashes before reaching the delete step.
type SandboxTagger interface {
	TagSandbox(ctx context.Context, sandboxID string, metadata map[string]string) error
}

type Runner struct {
	Config  Config
	Locker  lock.Lock
	Metrics metrics.Provider
	Clock   func() time.Time
	Tagger  SandboxTagger // optional; if nil, metadata tagging is skipped
}

type RunResult struct {
	Err        error
	FailedStep string
	SandboxID  string
}

type sandboxIDTracker struct {
	mu           sync.Mutex
	id           string
	err          error
	onDiscovered func(string)
}

func newSandboxIDTracker(onDiscovered func(string)) *sandboxIDTracker {
	return &sandboxIDTracker{onDiscovered: onDiscovered}
}

func (t *sandboxIDTracker) Set(id string) bool {
	cleanID := strings.TrimSpace(id)
	if cleanID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id == "" {
		t.id = cleanID
		if t.onDiscovered != nil {
			t.onDiscovered(cleanID)
		}
		return true
	}
	return false
}

func (t *sandboxIDTracker) Get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.id
}

func (t *sandboxIDTracker) SetError(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		t.err = err
	}
}

func (t *sandboxIDTracker) GetError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (r Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r Runner) metricsProvider() metrics.Provider {
	if r.Metrics != nil {
		return r.Metrics
	}
	return metrics.NoopProvider{}
}

func (r Runner) Run(ctx context.Context) error {
	runTimeout := r.Config.BaseConfig.RunTimeout
	if runTimeout <= 0 {
		runTimeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	target, env, region := resolveTargetTuple(r.Config.BaseConfig)

	lockTTL := r.Config.BaseConfig.LockTTL
	if lockTTL <= 0 {
		lockTTL = 10 * time.Minute
	}

	if r.Locker != nil {
		lockKey := fmt.Sprintf("%s-ui", target)
		outcome, lease, err := r.Locker.Acquire(ctx, lockKey, lockTTL)
		if err != nil {
			return fmt.Errorf("acquire lock: %w", err)
		}
		if outcome == lock.OutcomeAlreadyRunning {
			r.metricsProvider().RecordOverlapSkip(ctx, env, region, target)
			log.Info().Str("target", target).Msg("UI canary skipped because another run holds the target lease")
			return nil
		}
		if lease != nil {
			defer func() {
				if relErr := lease.Release(context.Background()); relErr != nil {
					log.Warn().Err(relErr).Msg("failed to release lock lease")
				}
			}()
		}
	}

	scenario := "ui-lifecycle"

	r.metricsProvider().RecordExecutionDelta(ctx, env, region, target, scenario, 1)
	defer r.metricsProvider().RecordExecutionDelta(ctx, env, region, target, scenario, -1)

	runID := fmt.Sprintf("ui-%d-%s", r.now().Unix(), uuid.NewString()[:8])
	start := r.now()
	result := "failure"

	log.Info().
		Str("run_id", runID).
		Str("target", target).
		Str("console_url", r.Config.ConsoleURL).
		Str("scenario", scenario).
		Msg("UI lifecycle canary started")

	res := r.runLifecycle(ctx, runID)
	err := res.Err
	if err == nil {
		result = "success"
	}
	duration := r.now().Sub(start)
	r.metricsProvider().RecordRun(ctx, env, region, target, scenario, result, duration)

	if err != nil {
		log.Error().
			Err(err).
			Str("run_id", runID).
			Str("failed_step", res.FailedStep).
			Str("sandbox_id", res.SandboxID).
			Dur("duration", duration).
			Msg("UI lifecycle canary failed")
		return err
	}

	log.Info().
		Str("run_id", runID).
		Str("sandbox_id", res.SandboxID).
		Dur("duration", duration).
		Msg("UI lifecycle canary completed successfully")
	return nil
}

func resolveTargetTuple(baseCfg config.Config) (target, env, region string) {
	target = baseCfg.Target
	if target == "" {
		target = "staging-us-central1"
	}
	env = baseCfg.Environment
	region = baseCfg.Region
	if (env == "" || region == "") && target != "" {
		parts := strings.Split(target, "-")
		if len(parts) >= 2 {
			if env == "" {
				env = parts[0]
			}
			if region == "" {
				region = strings.Join(parts[1:], "-")
			}
		}
	}
	return target, env, region
}

func (r Runner) runLifecycle(ctx context.Context, runID string) (res RunResult) {
	mp := r.metricsProvider()
	target, env, region := resolveTargetTuple(r.Config.BaseConfig)
	scenario := "ui-lifecycle"

	_ = playwright.Install(&playwright.RunOptions{
		SkipInstallBrowsers: true,
	})
	pw, err := playwright.Run()
	if err != nil {
		res.Err = fmt.Errorf("initialize playwright: %w", err)
		res.FailedStep = "driver_init"
		return res
	}
	defer func() {
		if stopErr := pw.Stop(); stopErr != nil {
			log.Warn().Err(stopErr).Msg("playwright stop error")
		}
	}()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(r.Config.Headless),
	})
	if err != nil {
		res.Err = fmt.Errorf("launch chromium: %w", err)
		res.FailedStep = "browser_launch"
		return res
	}
	defer browser.Close()

	contextOpts := playwright.BrowserNewContextOptions{
		Viewport: &playwright.Size{Width: 1280, Height: 800},
	}

	bCtx, err := browser.NewContext(contextOpts)
	if err != nil {
		res.Err = fmt.Errorf("create browser context: %w", err)
		res.FailedStep = "browser_context"
		return res
	}
	defer bCtx.Close()

	if r.Config.VercelProtectionBypass != "" {
		consoleParsed, pErr := url.Parse(r.Config.ConsoleURL)
		if pErr == nil {
			err = bCtx.Route("**/*", func(route playwright.Route) {
				req := route.Request()
				reqURL, err := url.Parse(req.URL())
				if err == nil && reqURL.Scheme == consoleParsed.Scheme && reqURL.Host == consoleParsed.Host {
					headers := req.Headers()
					headers["x-vercel-protection-bypass"] = r.Config.VercelProtectionBypass
					headers["x-vercel-set-bypass-cookie"] = "true"
					_ = route.Continue(playwright.RouteContinueOptions{
						Headers: headers,
					})
					return
				}
				_ = route.Continue()
			})
			if err != nil {
				res.Err = fmt.Errorf("configure vercel bypass route: %w", err)
				res.FailedStep = "browser_context"
				return res
			}
		}
	}

	page, err := bCtx.NewPage()
	if err != nil {
		res.Err = fmt.Errorf("create page: %w", err)
		res.FailedStep = "page_init"
		return res
	}
	defer page.Close()

	// Diagnostic artifact capture helper
	captureArtifacts := func(stepName string) {
		if r.Config.ArtifactsDir == "" {
			return
		}
		_ = os.MkdirAll(r.Config.ArtifactsDir, 0755)
		screenshotPath := filepath.Join(r.Config.ArtifactsDir, fmt.Sprintf("failure-%s-%s.png", stepName, runID))
		if _, err := page.Screenshot(playwright.PageScreenshotOptions{
			Path:     playwright.String(screenshotPath),
			FullPage: playwright.Bool(true),
		}); err != nil {
			log.Warn().Err(err).Str("screenshot", screenshotPath).Msg("failed to capture failure screenshot")
		} else {
			log.Info().Str("screenshot", screenshotPath).Msg("captured failure screenshot")
		}
	}

	stepTimeout := r.Config.StepTimeout
	stepTimeoutMs := float64(stepTimeout.Milliseconds())

	var (
		cleanupMu          sync.Mutex
		createdSandboxID   string
		createdSandboxName string
		sandboxDeleted     bool
	)
	setCreatedID := func(id string) {
		cleanupMu.Lock()
		defer cleanupMu.Unlock()
		if createdSandboxID == "" && id != "" {
			createdSandboxID = id
		}
	}
	getCreatedID := func() string {
		cleanupMu.Lock()
		defer cleanupMu.Unlock()
		return createdSandboxID
	}
	markDeleted := func() {
		cleanupMu.Lock()
		defer cleanupMu.Unlock()
		sandboxDeleted = true
	}

	// Guaranteed deferred cleanup if sandbox was created but not successfully deleted
	defer func() {
		cleanupMu.Lock()
		idToClean := createdSandboxID
		nameToClean := createdSandboxName
		isDel := sandboxDeleted
		cleanupMu.Unlock()

		if idToClean != "" && !isDel {
			log.Info().Str("sandbox_id", idToClean).Str("sandbox_name", nameToClean).Msg("executing deferred sandbox cleanup on failure/exit")
			_ = r.deleteSandboxInUI(page, idToClean, nameToClean, stepTimeoutMs)
		}
	}()

	// Step 1: Authenticate
	authStart := r.now()
	log.Info().Msg("UI step: authenticate")
	if err := Authenticate(ctx, page, r.Config); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "authenticate", "failure", r.now().Sub(authStart))
		captureArtifacts("authenticate")
		res.Err = fmt.Errorf("authenticate: %w", err)
		res.FailedStep = "authenticate"
		return res
	}
	mp.RecordStep(ctx, env, region, target, scenario, "authenticate", "success", r.now().Sub(authStart))

	// Step 2: Create Sandbox
	createStart := r.now()
	log.Info().Msg("UI step: create_sandbox")
	sandboxName := fmt.Sprintf("ui-canary-%d", r.now().Unix())
	cleanupMu.Lock()
	createdSandboxName = sandboxName
	cleanupMu.Unlock()

	sbID, err := r.createSandboxInUI(page, sandboxName, stepTimeoutMs, func(id string) {
		setCreatedID(id)
	})
	activeID := getCreatedID()
	if sbID != "" && activeID == "" {
		setCreatedID(sbID)
		activeID = sbID
	}
	res.SandboxID = activeID

	if err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "create_sandbox", "failure", r.now().Sub(createStart))
		captureArtifacts("create_sandbox")
		res.Err = fmt.Errorf("create sandbox: %w", err)
		res.FailedStep = "create_sandbox"
		return res
	}

	// Tag the sandbox with ownership metadata so the janitor can reap it if this
	// run crashes before the delete step.
	// Contract: Must retry up to 3 times. If tagging fails, delete the unowned sandbox
	// synchronously and abort the canary run to prevent orphaned unowned sandboxes.
	if r.Tagger != nil && activeID != "" {
		tagStart := r.now()
		tagMeta := sandboxmetadata.LegacyCanaryMetadata(
			env, region, target, runID,
			r.now(),
			r.now().Add(r.Config.BaseConfig.RetainFailedSandboxTTL),
		)
		var tagErr error
		const maxTagAttempts = 3
		for attempt := 1; attempt <= maxTagAttempts; attempt++ {
			tagErr = r.Tagger.TagSandbox(ctx, activeID, tagMeta)
			if tagErr == nil {
				log.Debug().Str("sandbox_id", activeID).Int("attempt", attempt).Msg("sandbox tagged with canary ownership metadata")
				break
			}
			log.Warn().Err(tagErr).Str("sandbox_id", activeID).Int("attempt", attempt).Msg("retryable failure tagging sandbox metadata")
			if attempt < maxTagAttempts {
				select {
				case <-ctx.Done():
					tagErr = ctx.Err()
				case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
				}
				if ctx.Err() != nil {
					break
				}
			}
		}
		if tagErr != nil {
			log.Error().Err(tagErr).Str("sandbox_id", activeID).Msg("failed to tag sandbox metadata after retries; deleting unowned sandbox synchronously to avoid orphan leak")
			mp.RecordStep(ctx, env, region, target, scenario, "tag_sandbox", "failure", r.now().Sub(tagStart))
			captureArtifacts("tag_sandbox")

			// Synchronously delete the unowned sandbox immediately
			if delErr := r.deleteSandboxInUI(page, activeID, sandboxName, stepTimeoutMs); delErr == nil {
				markDeleted()
			} else {
				log.Error().Err(delErr).Str("sandbox_id", activeID).Msg("failed synchronous deletion of unowned sandbox after tag failure")
			}

			res.Err = fmt.Errorf("tag sandbox after %d attempts: %w", maxTagAttempts, tagErr)
			res.FailedStep = "tag_sandbox"
			return res
		}
		mp.RecordStep(ctx, env, region, target, scenario, "tag_sandbox", "success", r.now().Sub(tagStart))
	}

	// Step 2b: Wait for UI readiness (dismiss connect modal, navigate to detail, wait for Active)
	// Runs only after durable ownership tagging has completed.
	readyStart := r.now()
	if err := r.waitForSandboxReadyInUI(page, activeID, stepTimeoutMs); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "create_sandbox", "failure", r.now().Sub(createStart))
		captureArtifacts("create_sandbox")
		res.Err = fmt.Errorf("sandbox UI readiness: %w", err)
		res.FailedStep = "create_sandbox"
		return res
	}
	mp.RecordStep(ctx, env, region, target, scenario, "create_sandbox", "success", r.now().Sub(createStart))
	log.Info().Dur("duration", r.now().Sub(readyStart)).Msg("sandbox UI readiness confirmed")

	// Step 3: Interactive Terminal Execution
	termStart := r.now()
	log.Info().Msg("UI step: terminal_exec")
	if err := r.executeTerminalCommand(page, activeID, r.Config.TerminalTimeout); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "terminal_exec", "failure", r.now().Sub(termStart))
		captureArtifacts("terminal_exec")
		res.Err = fmt.Errorf("terminal execution: %w", err)
		res.FailedStep = "terminal_exec"
		return res
	}
	mp.RecordStep(ctx, env, region, target, scenario, "terminal_exec", "success", r.now().Sub(termStart))

	// Step 4: Pause Sandbox
	pauseStart := r.now()
	log.Info().Msg("UI step: pause_sandbox")
	if err := r.pauseSandboxInUI(page, activeID, stepTimeoutMs); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "pause_sandbox", "failure", r.now().Sub(pauseStart))
		captureArtifacts("pause_sandbox")
		res.Err = fmt.Errorf("pause sandbox: %w", err)
		res.FailedStep = "pause_sandbox"
		return res
	}
	mp.RecordStep(ctx, env, region, target, scenario, "pause_sandbox", "success", r.now().Sub(pauseStart))

	// Step 5: Resume Sandbox
	resumeStart := r.now()
	log.Info().Msg("UI step: resume_sandbox")
	if err := r.resumeSandboxInUI(page, activeID, stepTimeoutMs); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "resume_sandbox", "failure", r.now().Sub(resumeStart))
		captureArtifacts("resume_sandbox")
		res.Err = fmt.Errorf("resume sandbox: %w", err)
		res.FailedStep = "resume_sandbox"
		return res
	}
	mp.RecordStep(ctx, env, region, target, scenario, "resume_sandbox", "success", r.now().Sub(resumeStart))

	// Step 6: Delete Sandbox
	deleteStart := r.now()
	log.Info().Msg("UI step: delete_sandbox")
	if err := r.deleteSandboxInUI(page, activeID, sandboxName, stepTimeoutMs); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "delete_sandbox", "failure", r.now().Sub(deleteStart))
		captureArtifacts("delete_sandbox")
		res.Err = fmt.Errorf("delete sandbox: %w", err)
		res.FailedStep = "delete_sandbox"
		return res
	}
	markDeleted()
	mp.RecordStep(ctx, env, region, target, scenario, "delete_sandbox", "success", r.now().Sub(deleteStart))

	return res
}

func (r Runner) createSandboxInUI(page playwright.Page, sandboxName string, timeoutMs float64, onIDDiscovered func(string)) (string, error) {
	// Navigate to sandboxes list if not already there
	if !strings.Contains(page.URL(), "/sandboxes") {
		listURL := r.Config.ConsoleURL + "/sandboxes/"
		log.Info().Str("url", listURL).Msg("navigating to sandboxes list")
		if _, err := page.Goto(listURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		}); err != nil {
			return "", fmt.Errorf("navigate to sandboxes: %w", err)
		}
	}

	// Trigger "Create sandbox" dialog
	log.Info().Msg("locating create sandbox trigger button")
	createBtn := page.Locator("button:has-text('Create sandbox'), button:has-text('Create Sandbox')").First()
	if err := createBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return "", fmt.Errorf("waiting for create sandbox button: %w", err)
	}
	if err := createBtn.Click(); err != nil {
		return "", fmt.Errorf("click create sandbox button: %w", err)
	}

	// Fill Sandbox Name in dialog (scope to dialog to avoid background search bar)
	log.Info().Msg("waiting for create sandbox dialog name input")
	nameInput := page.Locator("input[placeholder='my-sandbox'], div[role='dialog'] input, .dialog-popup input, div[data-state='open'] input").First()
	if err := nameInput.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return "", fmt.Errorf("waiting for sandbox name input: %w", err)
	}

	log.Info().Str("sandbox_name", sandboxName).Msg("filling sandbox name")
	_ = nameInput.Click()
	_ = nameInput.Fill("")
	_ = nameInput.Fill(sandboxName)

	// Wait for Create Sandbox submit button inside dialog
	submitDialogBtn := page.Locator("div[role='dialog'] button:has-text('Create Sandbox'), .dialog-popup button:has-text('Create Sandbox'), button:has-text('Create Sandbox')").Last()
	if err := submitDialogBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return "", fmt.Errorf("waiting for submit button in create dialog: %w", err)
	}

	// Ensure button is enabled by re-typing if React synthetic event was delayed
	for i := 0; i < 15; i++ {
		disabled, err := submitDialogBtn.IsDisabled()
		if err == nil && !disabled {
			break
		}
		_ = nameInput.Click()
		_ = nameInput.Fill(sandboxName)
		time.Sleep(200 * time.Millisecond)
	}

	tracker := newSandboxIDTracker(func(id string) {
		log.Info().Str("sandbox_id", id).Msg("discovered created sandbox ID")
		if onIDDiscovered != nil {
			onIDDiscovered(id)
		}
	})

	// Intercept backend creation response to grab ID immediately on wire asynchronously,
	// or capture rejection error code/message for fail-fast abort.
	// NOTE: Must run inside a goroutine to avoid deadlocking the Playwright message reader.
	responseHandler := func(res playwright.Response) {
		go func(resObj playwright.Response) {
			u := resObj.URL()
			cleanURL := strings.TrimRight(strings.Split(u, "?")[0], "/")
			isCreateEndpoint := cleanURL == "/sandboxes" || strings.HasSuffix(cleanURL, "/sandboxes")
			if isCreateEndpoint && resObj.Request().Method() == "POST" {
				if resObj.Status() >= 200 && resObj.Status() < 300 {
					body, err := resObj.Body()
					if err == nil {
						var data struct {
							ID string `json:"id"`
						}
						if err := json.Unmarshal(body, &data); err == nil && data.ID != "" {
							tracker.Set(data.ID)
						}
					}
				} else if resObj.Status() >= 400 {
					body, err := resObj.Body()
					var msg string
					if err == nil && len(body) > 0 {
						var errObj struct {
							Message string `json:"message"`
							Error   string `json:"error"`
							Detail  string `json:"detail"`
						}
						if json.Unmarshal(body, &errObj) == nil {
							if errObj.Message != "" {
								msg = errObj.Message
							} else if errObj.Error != "" {
								msg = errObj.Error
							} else if errObj.Detail != "" {
								msg = errObj.Detail
							}
						}
						if msg == "" {
							msg = strings.TrimSpace(string(body))
						}
					}
					if msg == "" {
						msg = fmt.Sprintf("HTTP %d %s", resObj.Status(), resObj.StatusText())
					}
					tracker.SetError(fmt.Errorf("backend rejected sandbox creation (%d): %s", resObj.Status(), msg))
				}
			}
		}(res)
	}
	page.On("response", responseHandler)
	defer page.RemoveListener("response", responseHandler)

	log.Info().Msg("submitting create sandbox dialog")
	if err := submitDialogBtn.Click(); err != nil {
		return "", fmt.Errorf("submit create sandbox dialog: %w", err)
	}
	log.Info().Msg("create sandbox submitted; awaiting connect dialog or navigation")

	// Locators for ConnectSandboxDialog actions
	openTerminalBtn := page.Locator("div[role='dialog'] button:has-text('Open Terminal'), .dialog-popup button:has-text('Open Terminal'), button:has-text('Open Terminal')").First()
	doneBtn := page.Locator("div[role='dialog'] button:has-text('Done'), .dialog-popup button:has-text('Done'), button:has-text('Done')").First()

	// Wait up to timeout for ConnectSandboxDialog, direct detail navigation, or table row appearance
	pollDeadline := r.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for r.now().Before(pollDeadline) {
		// Fail fast if backend rejected creation or if UI displays error toast/alert/validation
		if err := checkCreationError(tracker, page); err != nil {
			log.Error().Err(err).Msg("sandbox creation failed with rejection")
			return "", err
		}

		// 1. Check if ConnectSandboxDialog appeared with "Open Terminal"
		if count, _ := openTerminalBtn.Count(); count > 0 {
			if visible, _ := openTerminalBtn.IsVisible(); visible {
				if id := extractSandboxIDFromDialog(page); id != "" {
					tracker.Set(id)
					return tracker.Get(), nil
				}
			}
		}

		// 2. Check if URL already contains sandbox ID
		if id := extractSandboxIDFromURL(page.URL()); id != "" {
			tracker.Set(id)
			return tracker.Get(), nil
		}

		// 3. If Done button appeared without Open Terminal (fallback), inspect snippet then dismiss
		if count, _ := doneBtn.Count(); count > 0 {
			if visible, _ := doneBtn.IsVisible(); visible {
				if id := extractSandboxIDFromDialog(page); id != "" {
					tracker.Set(id)
					return tracker.Get(), nil
				}
			}
		}

		// 4. If row matching our sandboxName is in the table, click it to navigate to detail
		row := page.Locator(fmt.Sprintf("tr:has-text('%s'), div[role='row']:has-text('%s')", sandboxName, sandboxName)).First()
		if count, _ := row.Count(); count > 0 {
			_ = row.Click()
			time.Sleep(500 * time.Millisecond)
			if id := extractSandboxIDFromURL(page.URL()); id != "" {
				tracker.Set(id)
				return tracker.Get(), nil
			}
		}

		if id := tracker.Get(); id != "" {
			return id, nil
		}

		time.Sleep(200 * time.Millisecond)
	}

	// 5. Fallback: navigate directly to sandboxes list and find row only if no backend/UI errors
	if tracker.Get() == "" {
		if err := checkCreationError(tracker, page); err != nil {
			return "", err
		}

		listURL := fmt.Sprintf("%s/sandboxes/", r.Config.ConsoleURL)
		_, _ = page.Goto(listURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		})
		time.Sleep(1 * time.Second)
		row := page.Locator(fmt.Sprintf("tr:has-text('%s'), div[role='row']:has-text('%s')", sandboxName, sandboxName)).First()
		if count, _ := row.Count(); count > 0 {
			_ = row.Click()
			time.Sleep(500 * time.Millisecond)
			if id := extractSandboxIDFromURL(page.URL()); id != "" {
				tracker.Set(id)
			}
		}
	}

	finalID := tracker.Get()
	if finalID == "" {
		if err := checkCreationError(tracker, page); err != nil {
			return "", err
		}
		return "", fmt.Errorf("could not extract sandbox ID after creation")
	}

	return finalID, nil
}

func (r Runner) waitForSandboxReadyInUI(page playwright.Page, sandboxID string, timeoutMs float64) error {
	// 1. Dismiss ConnectSandboxDialog actions if still visible
	openTerminalBtn := page.Locator("div[role='dialog'] button:has-text('Open Terminal'), .dialog-popup button:has-text('Open Terminal'), button:has-text('Open Terminal')").First()
	doneBtn := page.Locator("div[role='dialog'] button:has-text('Done'), .dialog-popup button:has-text('Done'), button:has-text('Done')").First()
	if count, _ := openTerminalBtn.Count(); count > 0 {
		if visible, _ := openTerminalBtn.IsVisible(); visible {
			_ = openTerminalBtn.Click()
			_ = page.WaitForURL(fmt.Sprintf("%s/sandboxes/%s/**", r.Config.ConsoleURL, sandboxID), playwright.PageWaitForURLOptions{
				Timeout: playwright.Float(5000),
			})
		}
	} else if count, _ := doneBtn.Count(); count > 0 {
		if visible, _ := doneBtn.IsVisible(); visible {
			_ = doneBtn.Click()
		}
	}

	// 2. Navigate to sandbox detail page if not already on a sandbox page
	if !strings.HasPrefix(page.URL(), fmt.Sprintf("%s/sandboxes/%s", r.Config.ConsoleURL, sandboxID)) {
		detailURL := fmt.Sprintf("%s/sandboxes/%s/", r.Config.ConsoleURL, sandboxID)
		if _, err := page.Goto(detailURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		}); err != nil {
			return fmt.Errorf("navigate to detail page %s: %w", detailURL, err)
		}
	}

	// 3. Wait for "Active" status indicator in SandboxStatusHero or Terminal header,
	// periodically dispatching window focus event to prompt React Query to refetch
	log.Info().Str("sandbox_id", sandboxID).Msg("waiting for sandbox to become active")
	activeDeadline := r.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for r.now().Before(activeDeadline) {
		if uiErr := checkForUIError(page); uiErr != "" {
			return fmt.Errorf("sandbox active state error: %s", uiErr)
		}
		activeBadge := page.Locator("section:has-text('Active'), span:has-text('Active'), td:has-text('Active'), div:has-text('Active')").First()
		if count, _ := activeBadge.Count(); count > 0 {
			if visible, _ := activeBadge.IsVisible(); visible {
				log.Info().Str("sandbox_id", sandboxID).Msg("sandbox is Active")
				return nil
			}
		}
		time.Sleep(1 * time.Second)
		_, _ = page.Evaluate("() => window.dispatchEvent(new Event('focus'))")
	}

	return fmt.Errorf("waiting for sandbox %s to become active timed out", sandboxID)
}

func (r Runner) executeTerminalCommand(page playwright.Page, sandboxID string, timeout time.Duration) error {
	timeoutMs := float64(timeout.Milliseconds())

	// Navigate to terminal page if not already there
	terminalURL := fmt.Sprintf("%s/sandboxes/%s/terminal/", r.Config.ConsoleURL, sandboxID)
	if !strings.Contains(page.URL(), "/terminal") {
		if _, err := page.Goto(terminalURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		}); err != nil {
			return fmt.Errorf("navigate to terminal: %w", err)
		}
	}

	// Wait for xterm container to be present
	xtermContainer := page.Locator(".xterm, .xterm-screen, .xterm-rows").First()
	if err := xtermContainer.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for terminal xterm container: %w", err)
	}

	// Helper to extract current terminal buffer or text
	extractTerminalText := func() string {
		evalResult, evalErr := page.Evaluate(`() => {
			// 1. Try React Fiber hooks (termRef / serializeRef)
			for (const node of document.querySelectorAll('*')) {
				const key = Object.keys(node).find(k => k.startsWith('__reactFiber') || k.startsWith('__reactInternalInstance'));
				if (!key) continue;
				let fiber = node[key];
				while (fiber) {
					let hook = fiber.memoizedState;
					while (hook) {
						if (hook.memoizedState && typeof hook.memoizedState === 'object') {
							const state = hook.memoizedState;
							if (state.current) {
								if (typeof state.current.serialize === 'function') {
									try { return state.current.serialize(); } catch (e) {}
								}
								if (state.current.buffer && state.current.buffer.active) {
									const buf = state.current.buffer.active;
									let lines = [];
									for (let i = 0; i < buf.length; i++) {
										const line = buf.getLine(i);
										if (line) lines.push(line.translateToString(true));
									}
									return lines.join('\n');
								}
							}
						}
						hook = hook.next;
					}
					fiber = fiber.return;
				}
			}

			// 2. Try DOM _xterm property
			for (const node of document.querySelectorAll('*')) {
				if (node._xterm && node._xterm.buffer && node._xterm.buffer.active) {
					const buf = node._xterm.buffer.active;
					let lines = [];
					for (let i = 0; i < buf.length; i++) {
						const line = buf.getLine(i);
						if (line) lines.push(line.translateToString(true));
					}
					return lines.join('\n');
				}
			}

			// 3. Fallback: DOM textContent
			const el = document.querySelector('.xterm-rows') || document.querySelector('.xterm-accessibility') || document.querySelector('.xterm') || document.body;
			return el ? (el.textContent || el.innerText || '') : '';
		}`)
		if evalErr == nil {
			if s, ok := evalResult.(string); ok {
				return s
			}
		}
		text, _ := page.Locator(".xterm-rows, .xterm, div.xterm-screen").First().TextContent()
		return text
	}

	// 1. Wait for terminal WebSocket to connect and prompt to be ready (recovering from any transient "connection lost")
	promptTimeout := 30 * time.Second
	if timeout > 0 && timeout < promptTimeout {
		promptTimeout = timeout
	}
	readyDeadline := r.now().Add(promptTimeout)
	promptReady := false
	for r.now().Before(readyDeadline) {
		text := extractTerminalText()
		// If connected and has shell prompt
		if strings.Contains(text, "root@") || strings.Contains(text, "#") || strings.Contains(text, "$") {
			promptReady = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if !promptReady {
		log.Warn().Str("sandbox_id", sandboxID).Dur("timeout", promptTimeout).Msg("shell prompt did not appear within prompt timeout, proceeding to attempt execution")
	}

	helperTextarea := page.Locator(".xterm-helper-textarea").First()

	// Focus the terminal by clicking its dynamic bounding box center and focusing helper textarea
	focusTerminal := func() {
		if box, err := xtermContainer.BoundingBox(); err == nil && box != nil {
			_ = page.Mouse().Click(box.X+box.Width/2, box.Y+box.Height/2)
		} else {
			_ = xtermContainer.Click()
		}
		_, _ = page.Evaluate("() => { const ta = document.querySelector('.xterm-helper-textarea'); if (ta) { ta.focus(); } }")
		if count, _ := helperTextarea.Count(); count > 0 {
			_ = helperTextarea.Focus()
		}
	}

	focusTerminal()
	time.Sleep(500 * time.Millisecond)

	// Generate arithmetic transformation operands so typed keystrokes cannot false-positive match the evaluated output
	cmd, expectedOutput := generateTerminalCommand()

	// Send echo command function
	sendCommand := func() error {
		focusTerminal()
		if err := page.Keyboard().Type(cmd, playwright.KeyboardTypeOptions{Delay: playwright.Float(30)}); err != nil {
			return fmt.Errorf("type command to terminal: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
		if err := page.Keyboard().Press("Enter"); err != nil {
			return fmt.Errorf("press Enter in terminal: %w", err)
		}
		return nil
	}

	if err := sendCommand(); err != nil {
		return err
	}

	// Poll terminal until transformed output appears, retrying command once if needed
	pollDeadline := r.now().Add(timeout)
	lastRetry := r.now()
	for r.now().Before(pollDeadline) {
		text := extractTerminalText()
		if strings.Contains(text, expectedOutput) {
			log.Info().Str("expected_output", expectedOutput).Msg("terminal verification verified transformed shell output")
			return nil
		}

		// If 5 seconds passed without seeing the output, try typing once more (e.g. if a reconnect occurred)
		if r.now().Sub(lastRetry) > 5*time.Second {
			_ = sendCommand()
			lastRetry = r.now()
		}

		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("terminal output did not contain expected transformed sentinel %q within timeout", expectedOutput)
}

func (r Runner) pauseSandboxInUI(page playwright.Page, sandboxID string, timeoutMs float64) error {
	detailURL := fmt.Sprintf("%s/sandboxes/%s/", r.Config.ConsoleURL, sandboxID)
	if !strings.HasPrefix(page.URL(), fmt.Sprintf("%s/sandboxes/%s", r.Config.ConsoleURL, sandboxID)) || strings.Contains(page.URL(), "/terminal") {
		if _, err := page.Goto(detailURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		}); err != nil {
			return fmt.Errorf("navigate to sandbox detail: %w", err)
		}
	}

	// Click "Stop" button
	stopBtn := page.Locator("button:has-text('Stop')").First()
	if err := stopBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for Stop button: %w", err)
	}
	if err := stopBtn.Click(); err != nil {
		return fmt.Errorf("click Stop button: %w", err)
	}

	// Wait for status hero to report "Paused", checking for UI error alerts/toasts
	pausedBadge := page.Locator("section:has-text('Paused'), span:has-text('Paused'), td:has-text('Paused')").First()
	pollDeadline := r.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for r.now().Before(pollDeadline) {
		if uiErr := checkForUIError(page); uiErr != "" {
			return fmt.Errorf("pause sandbox rejected: %s", uiErr)
		}
		if count, _ := pausedBadge.Count(); count > 0 {
			if visible, _ := pausedBadge.IsVisible(); visible {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("waiting for Paused status timed out")
}

func (r Runner) resumeSandboxInUI(page playwright.Page, sandboxID string, timeoutMs float64) error {
	detailURL := fmt.Sprintf("%s/sandboxes/%s/", r.Config.ConsoleURL, sandboxID)
	if !strings.HasPrefix(page.URL(), fmt.Sprintf("%s/sandboxes/%s", r.Config.ConsoleURL, sandboxID)) || strings.Contains(page.URL(), "/terminal") {
		if _, err := page.Goto(detailURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		}); err != nil {
			return fmt.Errorf("navigate to sandbox detail for resume: %w", err)
		}
	}

	// Click "Start" button
	startBtn := page.Locator("button:has-text('Start')").First()
	if err := startBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for Start button: %w", err)
	}
	if err := startBtn.Click(); err != nil {
		return fmt.Errorf("click Start button: %w", err)
	}

	// Wait for status hero to report "Active", checking for UI error alerts/toasts
	activeBadge := page.Locator("section:has-text('Active'), #hero:has-text('Active'), span[data-status='active'], span[data-status='Active']").First()
	pollDeadline := r.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for r.now().Before(pollDeadline) {
		if uiErr := checkForUIError(page); uiErr != "" {
			return fmt.Errorf("resume sandbox rejected: %s", uiErr)
		}
		if count, _ := activeBadge.Count(); count > 0 {
			if visible, _ := activeBadge.IsVisible(); visible {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("waiting for Active status after resume timed out")
}

func (r Runner) deleteSandboxInUI(page playwright.Page, sandboxID, sandboxName string, timeoutMs float64) error {
	detailURL := fmt.Sprintf("%s/sandboxes/%s/", r.Config.ConsoleURL, sandboxID)
	if !strings.HasPrefix(page.URL(), fmt.Sprintf("%s/sandboxes/%s", r.Config.ConsoleURL, sandboxID)) || strings.Contains(page.URL(), "/terminal") {
		if _, err := page.Goto(detailURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		}); err != nil {
			return fmt.Errorf("navigate to sandbox detail for deletion: %w", err)
		}
	}

	// Open More actions menu
	menuTrigger := page.Locator("button[aria-label='More actions']").First()
	if err := menuTrigger.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		// Fallback: look for direct Delete button
		directDeleteBtn := page.Locator("button:has-text('Delete')").First()
		if count, _ := directDeleteBtn.Count(); count > 0 {
			_ = directDeleteBtn.Click()
		} else {
			return fmt.Errorf("waiting for actions menu: %w", err)
		}
	} else {
		if err := menuTrigger.Click(); err != nil {
			return fmt.Errorf("click actions menu trigger: %w", err)
		}

		// Click "Delete sandbox" menu item
		deleteMenuItem := page.Locator("div[role='menuitem']:has-text('Delete sandbox'), button:has-text('Delete sandbox')").First()
		if err := deleteMenuItem.WaitFor(playwright.LocatorWaitForOptions{
			State:   playwright.WaitForSelectorStateVisible,
			Timeout: playwright.Float(timeoutMs),
		}); err != nil {
			return fmt.Errorf("waiting for Delete sandbox menu item: %w", err)
		}
		if err := deleteMenuItem.Click(); err != nil {
			return fmt.Errorf("click Delete sandbox menu item: %w", err)
		}
	}

	// Delete confirmation dialog: type expected sandbox name
	dialog := page.Locator("div[role='dialog'], [role='alertdialog'], .dialog-popup, #delete-dialog").First()
	if err := dialog.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for delete confirmation dialog: %w", err)
	}

	confirmInput := dialog.Locator("input").First()
	if err := confirmInput.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for delete confirmation input: %w", err)
	}
	_ = confirmInput.Click()
	_ = confirmInput.Fill("")
	_ = confirmInput.PressSequentially(sandboxName, playwright.LocatorPressSequentiallyOptions{Delay: playwright.Float(30)})

	// Wait for the Delete button to become enabled and click it
	deleteBtn := dialog.Locator("button:has-text('Delete')").First()
	deadline := r.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	clicked := false
	for r.now().Before(deadline) {
		if uiErr := checkForUIError(page); uiErr != "" {
			return fmt.Errorf("delete sandbox rejected: %s", uiErr)
		}
		disabled, err := deleteBtn.IsDisabled()
		if err == nil && !disabled {
			if err := deleteBtn.Click(); err == nil {
				clicked = true
				break
			}
		}
		// Fallback re-fill if React state didn't pick up typing
		_ = confirmInput.Fill(sandboxName)
		time.Sleep(300 * time.Millisecond)
	}

	if !clicked {
		return fmt.Errorf("confirm delete button remained disabled or could not be clicked")
	}

	// Wait for the delete dialog to close while checking for UI rejection toasts
	dialogCloseDeadline := r.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	dialogClosed := false
	for r.now().Before(dialogCloseDeadline) {
		if uiErr := checkForUIError(page); uiErr != "" {
			return fmt.Errorf("delete sandbox rejected: %s", uiErr)
		}
		if hidden, _ := dialog.IsHidden(); hidden {
			dialogClosed = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !dialogClosed {
		if uiErr := checkForUIError(page); uiErr != "" {
			return fmt.Errorf("delete sandbox rejected: %s", uiErr)
		}
		return fmt.Errorf("delete confirmation dialog failed to close within %vms; deletion unconfirmed", timeoutMs)
	}

	// Wait for navigation away from the detail page back to the sandboxes dashboard
	listURL := fmt.Sprintf("%s/sandboxes/", r.Config.ConsoleURL)
	if err := page.WaitForURL(listURL+"**", playwright.PageWaitForURLOptions{
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		_, _ = page.Goto(listURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		})
	}

	// Verify that the sandboxes list view is loaded before evaluating row absence as deletion proof
	listLoadedDeadline := r.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	listLoaded := false
	for r.now().Before(listLoadedDeadline) {
		if uiErr := checkForUIError(page); uiErr != "" {
			return fmt.Errorf("sandboxes list load failed: %s", uiErr)
		}
		listIndicator := page.Locator("table, div[role='rowgroup'], :has-text('No Sandboxes'), :has-text('No sandboxes match'), button:has-text('Create sandbox'), button:has-text('Create Sandbox')").First()
		if isVisibleWithText(listIndicator) {
			listLoaded = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !listLoaded {
		return fmt.Errorf("sandboxes list failed to load after deletion")
	}

	// Poll until deleted sandbox is verified gone from the dashboard table
	time.Sleep(1 * time.Second)
	pollDeadline := r.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for r.now().Before(pollDeadline) {
		if uiErr := checkForUIError(page); uiErr != "" {
			return fmt.Errorf("delete sandbox rejected: %s", uiErr)
		}
		row := page.Locator(fmt.Sprintf("tr:has-text('%s'), div[role='row']:has-text('%s')", sandboxName, sandboxName)).First()
		count, _ := row.Count()
		if count == 0 {
			return nil
		}
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("sandbox %s still present in table after deletion", sandboxName)
}

func extractSandboxIDFromURL(rawURL string) string {
	parts := strings.Split(rawURL, "/")
	for i, part := range parts {
		if part == "sandboxes" && i+1 < len(parts) {
			id := parts[i+1]
			// Trim query or trailing slash
			if idx := strings.IndexAny(id, "?#"); idx != -1 {
				id = id[:idx]
			}
			return strings.TrimSpace(id)
		}
	}
	return ""
}

var connectSandboxRegex = regexp.MustCompile(`Sandbox\.connect\(\s*["']([^"']+)["']`)

func extractSandboxIDFromDialog(page playwright.Page) string {
	dialogs := page.Locator("div[role='dialog'], .dialog-popup")
	count, _ := dialogs.Count()
	for i := 0; i < count; i++ {
		d := dialogs.Nth(i)
		if visible, _ := d.IsVisible(); visible {
			if text, err := d.TextContent(); err == nil {
				if match := connectSandboxRegex.FindStringSubmatch(text); len(match) > 1 {
					return match[1]
				}
			}
		}
	}
	return ""
}

const terminalSentinelPrefix = "RES_UI_"

func generateTerminalCommand() (command string, expectedOutput string) {
	randNonce := func() int { return 1000 + rand.Intn(9000) }
	nonceA := randNonce()
	nonceB := randNonce()
	for nonceB == nonceA {
		nonceB = randNonce()
	}
	command = fmt.Sprintf(`echo "%s$((%d + %d))"`, terminalSentinelPrefix, nonceA, nonceB)
	expectedOutput = fmt.Sprintf("%s%d", terminalSentinelPrefix, nonceA+nonceB)
	return command, expectedOutput
}

func isVisibleWithText(loc playwright.Locator) bool {
	if count, err := loc.Count(); err != nil || count == 0 {
		return false
	}
	visible, err := loc.IsVisible()
	return err == nil && visible
}

func checkForUIError(page playwright.Page) string {
	// 1. Explicit Sonner error toast
	sonnerToasts := page.Locator("[data-sonner-toast][data-type='error'], [data-sonner-toast]:has(.text-destructive)")
	if count, _ := sonnerToasts.Count(); count > 0 {
		for i := 0; i < count; i++ {
			toast := sonnerToasts.Nth(i)
			if isVisibleWithText(toast) {
				if txt, err := toast.InnerText(); err == nil {
					clean := strings.TrimSpace(txt)
					if clean != "" && clean != "*" {
						return clean
					}
				}
			}
		}
	}

	// 2. Destructive alert callouts: role='alert' that contains destructive styling or is inside a modal dialog
	alerts := page.Locator("[role='alert'].text-destructive, [role='alert'].border-destructive, [role='alert']:has(.text-destructive), [role='alert'][data-variant='destructive'], div[role='dialog'] [role='alert'], div[role='alertdialog'] [role='alert']")
	if count, _ := alerts.Count(); count > 0 {
		for i := 0; i < count; i++ {
			alert := alerts.Nth(i)
			if visible, err := alert.IsVisible(); err == nil && visible {
				if txt, err := alert.InnerText(); err == nil {
					clean := strings.TrimSpace(txt)
					if clean != "" && clean != "*" {
						return clean
					}
				}
			}
		}
	}

	return ""
}

func checkDialogValidationError(page playwright.Page) string {
	// Form error messages in dialog (excluding label asterisks and action buttons)
	dialogErrors := page.Locator("div[role='dialog'] p.text-destructive, div[role='dialog'] [role='alert'], div[role='dialog'] .text-destructive:not(label *):not(label):not(button), div[role='alertdialog'] p.text-destructive, div[role='alertdialog'] [role='alert'], div[role='alertdialog'] .text-destructive:not(label *):not(label):not(button)")
	count, _ := dialogErrors.Count()
	for i := 0; i < count; i++ {
		el := dialogErrors.Nth(i)
		if isVisibleWithText(el) {
			if txt, err := el.InnerText(); err == nil {
				clean := strings.TrimSpace(txt)
				if clean != "" && clean != "*" {
					return clean
				}
			}
		}
	}

	// Form inputs marked invalid
	invalidInput := page.Locator("div[role='dialog'] input[aria-invalid='true'], div[role='alertdialog'] input[aria-invalid='true']").First()
	if isVisibleWithText(invalidInput) {
		if parent := invalidInput.Locator("..").First(); isVisibleWithText(parent) {
			if txt, err := parent.InnerText(); err == nil {
				clean := strings.TrimSpace(txt)
				if clean != "" && clean != "*" {
					return fmt.Sprintf("invalid input: %s", clean)
				}
			}
		}
		return "form input marked invalid"
	}

	return ""
}

func checkCreationError(tracker *sandboxIDTracker, page playwright.Page) error {
	if backendErr := tracker.GetError(); backendErr != nil {
		return backendErr
	}
	if uiErr := checkForUIError(page); uiErr != "" {
		return fmt.Errorf("create sandbox rejected by UI: %s", uiErr)
	}
	if valErr := checkDialogValidationError(page); valErr != "" {
		return fmt.Errorf("create sandbox validation error: %s", valErr)
	}
	return nil
}
