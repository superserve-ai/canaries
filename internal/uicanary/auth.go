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
	log.Info().Str("email", maskEmail(cfg.Email)).Msg("authenticating UI canary via email/password form")

	baseURL := strings.TrimRight(cfg.ConsoleURL, "/")
	rootURL := baseURL + "/"
	timeoutMs := float64(cfg.StepTimeout.Milliseconds())

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

		// Check if redirected to authenticated dashboard
		if currentURL, err := url.Parse(page.URL()); err == nil {
			originMatch := pErr != nil || (currentURL.Scheme == parsedBase.Scheme && currentURL.Host == parsedBase.Host)
			if originMatch && isDashboardPath(currentURL.Path) && isDashboardContentVisible(page) {
				return nil
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	if currentURL, err := url.Parse(page.URL()); err == nil {
		originMatch := pErr != nil || (currentURL.Scheme == parsedBase.Scheme && currentURL.Host == parsedBase.Host)
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
