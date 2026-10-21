package uicanary

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"
	"github.com/rs/zerolog/log"
)

// Authenticate executes the email & password sign-in flow on the Superserve console.
func Authenticate(ctx context.Context, page playwright.Page, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	log.Info().Str("email", maskEmail(cfg.Email)).Msg("authenticating UI canary via email/password form")

	baseURL := strings.TrimRight(cfg.ConsoleURL, "/")
	rootURL := baseURL + "/"
	timeoutMs := browserTimeoutMs(ctx, float64(cfg.StepTimeout.Milliseconds()))
	page.SetDefaultTimeout(timeoutMs)

	if _, err := page.Goto(rootURL, playwright.PageGotoOptions{
		Timeout:   playwright.Float(timeoutMs),
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	}); err != nil {
		return fmt.Errorf("navigate to %s: %w", rootURL, err)
	}

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

	// Poll until redirected to authenticated dashboard, checking for error alerts
	parsedBase, pErr := url.Parse(baseURL)
	deadline := time.Now().Add(cfg.StepTimeout)
	for time.Now().Before(deadline) {
		// Dashboard status badges can use destructive styling (for example FAILED).
		// Once navigation reaches the dashboard, wait for its content instead of
		// interpreting those badges as sign-in errors.
		onDashboard := false
		if currentURL, err := url.Parse(page.URL()); err == nil {
			originMatch := pErr == nil && currentURL.Scheme == parsedBase.Scheme && currentURL.Host == parsedBase.Host
			onDashboard = originMatch && isDashboardPath(currentURL.Path)
			if onDashboard && isDashboardContentVisible(page) {
				return nil
			}
		}
		if !onDashboard {
			errorLoc := page.Locator(".text-destructive, [role='alert']")
			if count, _ := errorLoc.Count(); count > 0 {
				if visible, _ := errorLoc.First().IsVisible(); visible {
					errMsg, _ := errorLoc.First().InnerText()
					if errMsg = strings.TrimSpace(errMsg); errMsg != "" {
						return fmt.Errorf("sign in failed with message: %s", errMsg)
					}
				}
			}
		}

		if err := waitForBrowserPoll(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}

	if currentURL, err := url.Parse(page.URL()); err == nil {
		originMatch := pErr == nil && currentURL.Scheme == parsedBase.Scheme && currentURL.Host == parsedBase.Host
		if originMatch && isDashboardPath(currentURL.Path) && isDashboardContentVisible(page) {
			return nil
		}
	}

	return fmt.Errorf("sign in failed, still on %s", page.URL())
}

func isDashboardPath(path string) bool {
	cleanPath := strings.TrimRight(path, "/")
	return cleanPath == "/sandboxes" || strings.HasPrefix(cleanPath, "/sandboxes/")
}

func isDashboardContentVisible(page playwright.Page) bool {
	loc := page.Locator("h1:has-text('Sandboxes'), button:has-text('Create sandbox'), button:has-text('Create Sandbox'), :has-text('No Sandboxes'), table, div[role='rowgroup']").First()
	if count, _ := loc.Count(); count > 0 {
		visible, _ := loc.IsVisible()
		return visible
	}
	return false
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
