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
	log.Info().Str("email", maskEmail(cfg.Email)).Msg("authenticating UI canary via email/password form")

	baseURL := strings.TrimRight(cfg.ConsoleURL, "/")
	signinURL := baseURL + "/auth/signin?next=/sandboxes/"
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
	sandboxesURL := baseURL + "/sandboxes/"
	deadline := time.Now().Add(cfg.StepTimeout)
	fallbackAttempted := false
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

		// Fallback: If still on signin after initial wait and no error alert,
		// trigger a single navigation to sandboxes dashboard to recover if client SPA router stalled.
		if !fallbackAttempted && time.Now().After(deadline.Add(-cfg.StepTimeout+4*time.Second)) && strings.Contains(page.URL(), "/auth/signin") {
			fallbackAttempted = true
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

func maskEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return "***"
	}
	name, domain := parts[0], parts[1]
	if len(name) <= 2 {
		return "***@" + domain
	}
	return name[:1] + "***" + name[len(name)-1:] + "@" + domain
}
