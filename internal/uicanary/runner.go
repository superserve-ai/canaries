package uicanary

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/playwright-community/playwright-go"
	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/lock"
	"github.com/superserve-ai/canaries/internal/metrics"
)

type Runner struct {
	Config  Config
	Locker  lock.Lock
	Metrics metrics.Provider
	Clock   func() time.Time
}

type RunResult struct {
	Err        error
	FailedStep string
	SandboxID  string
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

	target := r.Config.BaseConfig.Target
	if target == "" {
		target = "staging-us-central1"
	}
	lockTTL := r.Config.BaseConfig.LockTTL
	if lockTTL <= 0 {
		lockTTL = 10 * time.Minute
	}

	if r.Locker != nil {
		outcome, lease, err := r.Locker.Acquire(ctx, target, lockTTL)
		if err != nil {
			return fmt.Errorf("acquire lock: %w", err)
		}
		if outcome == lock.OutcomeAlreadyRunning {
			r.metricsProvider().RecordOverlapSkip(ctx, r.Config.BaseConfig.Environment, r.Config.BaseConfig.Region, target)
			log.Info().Str("target", target).Msg("UI canary skipped because another run holds the target lock")
			return nil
		}
		defer func() {
			if lease != nil {
				if err := lease.Release(context.Background()); err != nil {
					log.Error().Err(err).Msg("release lock failed")
				}
			}
		}()
	}

	scenario := "ui-lifecycle"
	env := r.Config.BaseConfig.Environment
	region := r.Config.BaseConfig.Region

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

func (r Runner) runLifecycle(ctx context.Context, runID string) (res RunResult) {
	mp := r.metricsProvider()
	env := r.Config.BaseConfig.Environment
	region := r.Config.BaseConfig.Region
	target := r.Config.BaseConfig.Target
	scenario := "ui-lifecycle"

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

	bCtx, err := browser.NewContext(playwright.BrowserNewContextOptions{
		Viewport: &playwright.Size{Width: 1280, Height: 800},
	})
	if err != nil {
		res.Err = fmt.Errorf("create browser context: %w", err)
		res.FailedStep = "browser_context"
		return res
	}
	defer bCtx.Close()

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
		_, _ = page.Screenshot(playwright.PageScreenshotOptions{
			Path:     playwright.String(screenshotPath),
			FullPage: playwright.Bool(true),
		})
		log.Info().Str("screenshot", screenshotPath).Msg("captured failure screenshot")
	}

	stepTimeout := r.Config.StepTimeout
	stepTimeoutMs := float64(stepTimeout.Milliseconds())

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
	createdSandboxID, err := r.createSandboxInUI(page, sandboxName, stepTimeoutMs)
	if err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "create_sandbox", "failure", r.now().Sub(createStart))
		captureArtifacts("create_sandbox")
		res.Err = fmt.Errorf("create sandbox: %w", err)
		res.FailedStep = "create_sandbox"
		return res
	}
	res.SandboxID = createdSandboxID
	mp.RecordStep(ctx, env, region, target, scenario, "create_sandbox", "success", r.now().Sub(createStart))

	// Step 3: Interactive Terminal Execution
	termStart := r.now()
	log.Info().Msg("UI step: terminal_exec")
	termToken := fmt.Sprintf("UI_TOKEN_%s", runID)
	if err := r.executeTerminalCommand(page, res.SandboxID, termToken, r.Config.TerminalTimeout); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "terminal_exec", "failure", r.now().Sub(termStart))
		captureArtifacts("terminal_exec")
		res.Err = fmt.Errorf("terminal execution: %w", err)
		res.FailedStep = "terminal_exec"
		// Attempt best-effort cleanup on terminal failure
		_ = r.deleteSandboxInUI(page, res.SandboxID, sandboxName, stepTimeoutMs)
		return res
	}
	mp.RecordStep(ctx, env, region, target, scenario, "terminal_exec", "success", r.now().Sub(termStart))

	// Step 4: Pause Sandbox
	pauseStart := r.now()
	log.Info().Msg("UI step: pause_sandbox")
	if err := r.pauseSandboxInUI(page, res.SandboxID, stepTimeoutMs); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "pause_sandbox", "failure", r.now().Sub(pauseStart))
		captureArtifacts("pause_sandbox")
		res.Err = fmt.Errorf("pause sandbox: %w", err)
		res.FailedStep = "pause_sandbox"
		_ = r.deleteSandboxInUI(page, res.SandboxID, sandboxName, stepTimeoutMs)
		return res
	}
	mp.RecordStep(ctx, env, region, target, scenario, "pause_sandbox", "success", r.now().Sub(pauseStart))

	// Step 5: Resume Sandbox
	resumeStart := r.now()
	log.Info().Msg("UI step: resume_sandbox")
	if err := r.resumeSandboxInUI(page, res.SandboxID, stepTimeoutMs); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "resume_sandbox", "failure", r.now().Sub(resumeStart))
		captureArtifacts("resume_sandbox")
		res.Err = fmt.Errorf("resume sandbox: %w", err)
		res.FailedStep = "resume_sandbox"
		_ = r.deleteSandboxInUI(page, res.SandboxID, sandboxName, stepTimeoutMs)
		return res
	}
	mp.RecordStep(ctx, env, region, target, scenario, "resume_sandbox", "success", r.now().Sub(resumeStart))

	// Step 6: Delete Sandbox
	deleteStart := r.now()
	log.Info().Msg("UI step: delete_sandbox")
	if err := r.deleteSandboxInUI(page, res.SandboxID, sandboxName, stepTimeoutMs); err != nil {
		mp.RecordStep(ctx, env, region, target, scenario, "delete_sandbox", "failure", r.now().Sub(deleteStart))
		captureArtifacts("delete_sandbox")
		res.Err = fmt.Errorf("delete sandbox: %w", err)
		res.FailedStep = "delete_sandbox"
		return res
	}
	mp.RecordStep(ctx, env, region, target, scenario, "delete_sandbox", "success", r.now().Sub(deleteStart))

	return res
}

func (r Runner) createSandboxInUI(page playwright.Page, sandboxName string, timeoutMs float64) (string, error) {
	// Navigate to sandboxes list if not already there
	if !strings.Contains(page.URL(), "/sandboxes") {
		if _, err := page.Goto(r.Config.ConsoleURL+"/sandboxes/", playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		}); err != nil {
			return "", fmt.Errorf("navigate to sandboxes: %w", err)
		}
	}

	// Trigger "Create sandbox" dialog
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
	nameInput := page.Locator("div[role='dialog'] input, .dialog-popup input, div[data-state='open'] input, input[placeholder='my-sandbox']").First()
	if err := nameInput.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return "", fmt.Errorf("waiting for sandbox name input: %w", err)
	}

	// Focus, clear, and type sandbox name to trigger React synthetic state
	_ = nameInput.Click()
	_ = nameInput.Fill("")
	if err := nameInput.PressSequentially(sandboxName, playwright.LocatorPressSequentiallyOptions{Delay: playwright.Float(20)}); err != nil {
		_ = nameInput.Fill(sandboxName)
	}

	// Wait for enabled Create Sandbox submit button inside dialog
	submitDialogBtn := page.Locator("div[role='dialog'] button:has-text('Create Sandbox'):not([disabled]), .dialog-popup button:has-text('Create Sandbox'):not([disabled]), button:has-text('Create Sandbox'):not([disabled])").Last()
	if err := submitDialogBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		// Fallback: try filling again if state didn't catch
		_ = nameInput.Fill(sandboxName)
		_ = nameInput.Press("Tab")
	}

	if err := submitDialogBtn.Click(); err != nil {
		return "", fmt.Errorf("submit create sandbox dialog: %w", err)
	}

	var sandboxID string

	// Check if ConnectSandboxDialog appeared or if redirected to detail page
	doneBtn := page.Locator("button:has-text('Done')").First()

	// Wait up to timeout for direct detail navigation or table row appearance
	pollDeadline := r.now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for r.now().Before(pollDeadline) {
		// If connect dialog appeared, dismiss it
		if count, _ := doneBtn.Count(); count > 0 {
			_ = doneBtn.Click()
		}

		// 1. Check if URL already contains sandbox ID
		if id := extractSandboxIDFromURL(page.URL()); id != "" {
			sandboxID = id
			break
		}

		// 2. If row matching our sandboxName is in the table, click it to navigate to detail
		row := page.Locator(fmt.Sprintf("tr:has-text('%s'), div[role='row']:has-text('%s')", sandboxName, sandboxName)).First()
		if count, _ := row.Count(); count > 0 {
			_ = row.Click()
			time.Sleep(500 * time.Millisecond)
			if id := extractSandboxIDFromURL(page.URL()); id != "" {
				sandboxID = id
				break
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	if sandboxID == "" {
		return "", fmt.Errorf("could not extract sandbox ID after creation")
	}

	// Navigate to sandbox detail page if not already there
	detailURL := fmt.Sprintf("%s/sandboxes/%s/", r.Config.ConsoleURL, sandboxID)
	if !strings.HasPrefix(page.URL(), fmt.Sprintf("%s/sandboxes/%s", r.Config.ConsoleURL, sandboxID)) {
		if _, err := page.Goto(detailURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		}); err != nil {
			return sandboxID, fmt.Errorf("navigate to detail page %s: %w", detailURL, err)
		}
	}

	// Wait for "Active" status indicator in SandboxStatusHero
	activeBadge := page.Locator("section:has-text('Active'), span:has-text('Active'), td:has-text('Active')").First()
	if err := activeBadge.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return sandboxID, fmt.Errorf("waiting for sandbox to become Active: %w", err)
	}

	return sandboxID, nil
}

func (r Runner) executeTerminalCommand(page playwright.Page, sandboxID, token string, timeout time.Duration) error {
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
						if (hook.memoizedState && hook.memoizedState.current) {
							const val = hook.memoizedState.current;
							if (typeof val.serialize === 'function') {
								try { return val.serialize(); } catch (e) {}
							}
							if (val.buffer && val.buffer.active) {
								try {
									let lines = [];
									for (let i = 0; i < val.buffer.active.length; i++) {
										const l = val.buffer.active.getLine(i);
										if (l) lines.push(l.translateToString(true));
									}
									return lines.join('\n');
								} catch (e) {}
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
	readyDeadline := r.now().Add(30 * time.Second)
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
		log.Warn().Str("sandbox_id", sandboxID).Msg("shell prompt did not appear within 30s, proceeding to attempt execution")
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

	// Send echo command function
	sendCommand := func() error {
		focusTerminal()
		cmd := fmt.Sprintf("echo %s", token)
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

	// Poll terminal until token appears, retrying command once if needed
	pollDeadline := r.now().Add(timeout)
	lastRetry := r.now()
	for r.now().Before(pollDeadline) {
		text := extractTerminalText()
		if strings.Contains(text, token) {
			log.Info().Str("token", token).Msg("terminal verification verified token output")
			return nil
		}

		// If 5 seconds passed without seeing the token or prompt, try typing once more (e.g. if a reconnect occurred)
		if r.now().Sub(lastRetry) > 5*time.Second {
			_ = sendCommand()
			lastRetry = r.now()
		}

		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("terminal output did not contain expected verification token %q within timeout", token)
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

	// Wait for status hero to report "Paused"
	pausedBadge := page.Locator("section:has-text('Paused'), span:has-text('Paused'), td:has-text('Paused')").First()
	if err := pausedBadge.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for Paused status: %w", err)
	}

	return nil
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

	// Wait for status hero to report "Active"
	activeBadge := page.Locator("section:has-text('Active'), span:has-text('Active'), td:has-text('Active')").First()
	if err := activeBadge.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for Active status after resume: %w", err)
	}

	return nil
}

func (r Runner) deleteSandboxInUI(page playwright.Page, sandboxID, sandboxName string, timeoutMs float64) error {
	detailURL := fmt.Sprintf("%s/sandboxes/%s/", r.Config.ConsoleURL, sandboxID)
	if !strings.HasPrefix(page.URL(), fmt.Sprintf("%s/sandboxes/%s", r.Config.ConsoleURL, sandboxID)) || strings.Contains(page.URL(), "/terminal") {
		_, _ = page.Goto(detailURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(timeoutMs),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		})
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
	confirmInput := page.Locator("div[role='dialog'] input, .dialog-popup input, input[placeholder*='sandbox' i], #delete-dialog input, input#delete-input").First()
	if err := confirmInput.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for delete confirmation input: %w", err)
	}
	_ = confirmInput.Click()
	_ = confirmInput.Fill("")
	if err := confirmInput.PressSequentially(sandboxName, playwright.LocatorPressSequentiallyOptions{Delay: playwright.Float(20)}); err != nil {
		_ = confirmInput.Fill(sandboxName)
	}

	// Click destructive Delete button in dialog
	confirmDeleteBtn := page.Locator("div[role='dialog'] button:has-text('Delete'):not([disabled]), .dialog-popup button:has-text('Delete'):not([disabled]), #delete-dialog button").Last()
	if err := confirmDeleteBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		_ = confirmInput.Fill(sandboxName)
	}
	if err := confirmDeleteBtn.Click(); err != nil {
		return fmt.Errorf("click confirm delete button: %w", err)
	}

	// Verify navigation to the sandbox list page
	listURL := fmt.Sprintf("%s/sandboxes/", r.Config.ConsoleURL)
	if err := page.WaitForURL(listURL+"**", playwright.PageWaitForURLOptions{
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for redirect to sandbox list after delete: %w", err)
	}
	return nil
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
