package uicanary

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"
	"github.com/rs/zerolog/log"
)

// Authenticate executes the email & password sign-in flow on the Superserve console.
func Authenticate(ctx context.Context, page playwright.Page, cfg Config) error {
	log.Info().Str("email", cfg.Email).Msg("authenticating UI canary via email/password form")

	signinURL := cfg.ConsoleURL + "/auth/signin"
	if _, err := page.Goto(signinURL, playwright.PageGotoOptions{
		Timeout:   playwright.Float(float64(cfg.StepTimeout.Milliseconds())),
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	}); err != nil {
		return fmt.Errorf("navigate to %s: %w", signinURL, err)
	}

	timeoutMs := float64(cfg.StepTimeout.Milliseconds())

	emailInput := page.Locator("input[type='email'], input[placeholder*='Email' i], input[name='email']").First()
	if err := emailInput.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for email input: %w", err)
	}
	if err := emailInput.Fill(cfg.Email); err != nil {
		return fmt.Errorf("fill email: %w", err)
	}

	passwordInput := page.Locator("input[type='password'], input[placeholder*='Password' i], input[name='password']").First()
	if err := passwordInput.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	}); err != nil {
		return fmt.Errorf("waiting for password input: %w", err)
	}
	if err := passwordInput.Fill(cfg.Password); err != nil {
		return fmt.Errorf("fill password: %w", err)
	}

	submitBtn := page.Locator("button[type='submit'], button:has-text('Sign In'), button:has-text('SIGN IN')").First()
	if err := submitBtn.Click(); err != nil {
		return fmt.Errorf("click sign in: %w", err)
	}

	// Allow Supabase authentication request to complete and persist session tokens
	time.Sleep(3 * time.Second)

	// Poll until redirected to sandboxes, checking for error alerts
	sandboxesURL := cfg.ConsoleURL + "/sandboxes/"
	deadline := time.Now().Add(cfg.StepTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(page.URL(), "/sandboxes") {
			return nil
		}

		// Check if an error message is visible
		errorLoc := page.Locator(".text-destructive, [role='alert']")
		if count, _ := errorLoc.Count(); count > 0 {
			if visible, _ := errorLoc.First().IsVisible(); visible {
				errMsg, _ := errorLoc.First().InnerText()
				errMsg = strings.TrimSpace(errMsg)
				if errMsg != "" {
					return fmt.Errorf("sign in failed with message: %s", errMsg)
				}
			}
		}

		// Navigate to sandboxes dashboard
		if strings.Contains(page.URL(), "/auth/signin") {
			_, _ = page.Goto(sandboxesURL, playwright.PageGotoOptions{
				Timeout:   playwright.Float(timeoutMs),
				WaitUntil: playwright.WaitUntilStateDomcontentloaded,
			})
		}

		time.Sleep(1 * time.Second)
	}

	if strings.Contains(page.URL(), "/sandboxes") {
		return nil
	}
	return fmt.Errorf("sign in failed, still on %s", page.URL())
}
