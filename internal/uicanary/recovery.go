package uicanary

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/playwright-community/playwright-go"
	"github.com/rs/zerolog/log"
	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
)

type SandboxRecovery interface {
	ListSandboxes(context.Context, map[string]string) ([]canaryapi.Sandbox, error)
	DeleteSandbox(context.Context, string) error
}

func (r Runner) creationMetadata(runID string) map[string]string {
	target, env, region := resolveTargetTuple(r.Config.BaseConfig)
	now := r.now().UTC()
	return sandboxmetadata.LegacyCanaryMetadata(env, region, target, runID, now, now.Add(r.sandboxSafetyTimeout()))
}

func (r Runner) sandboxSafetyTimeout() time.Duration {
	timeout := r.Config.BaseConfig.RunTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	// Leave room for a normal run, then bound compute even if both cleanup paths fail.
	return timeout + 5*time.Minute
}

func (r Runner) configureCreationSafety(page playwright.Page, metadata map[string]string) error {
	dialog := page.Locator("div[role='dialog'], .dialog-popup").Filter(playwright.LocatorFilterOptions{Has: page.Locator("input[placeholder='my-sandbox']")}).First()
	if err := dialog.GetByRole(*playwright.AriaRoleButton, playwright.LocatorGetByRoleOptions{Name: "Advanced Options", Exact: playwright.Bool(true)}).Click(); err != nil {
		return fmt.Errorf("open sandbox safety settings: %w", err)
	}
	seconds := int64(r.sandboxSafetyTimeout() / time.Second)
	if err := dialog.Locator("input[placeholder='No timeout']").Fill(strconv.FormatInt(seconds, 10)); err != nil {
		return fmt.Errorf("set sandbox safety timeout: %w", err)
	}
	// Auto-delete is measured from pause; it must allow the pause/resume check to finish.
	if err := dialog.Locator("input[placeholder='Keep forever']").Fill("300"); err != nil {
		return fmt.Errorf("set sandbox safety deletion: %w", err)
	}
	metadataSection := dialog.Locator("div.space-y-2").Filter(playwright.LocatorFilterOptions{Has: page.Locator("span:text-is('Metadata')")}).Last()
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := metadataSection.GetByRole(*playwright.AriaRoleButton, playwright.LocatorGetByRoleOptions{Name: "Add", Exact: playwright.Bool(true)}).Click(); err != nil {
			return fmt.Errorf("add sandbox ownership field: %w", err)
		}
		if err := metadataSection.Locator("input[placeholder='key']").Last().Fill(key); err != nil {
			return fmt.Errorf("set sandbox ownership key: %w", err)
		}
		if err := metadataSection.Locator("input[placeholder='value']").Last().Fill(metadata[key]); err != nil {
			return fmt.Errorf("set sandbox ownership value: %w", err)
		}
	}
	return nil
}

func (r Runner) recoverSandbox(ctx context.Context, name string, metadata map[string]string) error {
	if r.Recovery == nil {
		return errors.New("API recovery is not configured")
	}
	query := sandboxmetadata.OwnershipQuery(metadata[sandboxmetadata.KeyEnvironment], metadata[sandboxmetadata.KeyManagedBy])
	query["metadata."+sandboxmetadata.KeyRunID] = metadata[sandboxmetadata.KeyRunID]
	query["metadata."+sandboxmetadata.KeyCanaryTarget] = metadata[sandboxmetadata.KeyCanaryTarget]
	items, err := r.Recovery.ListSandboxes(ctx, query)
	if err != nil {
		return fmt.Errorf("find run sandbox: %w", err)
	}
	var errs []error
	for _, item := range items {
		if item.ID == "" || item.Name != name {
			continue
		}
		owned := true
		for key, value := range metadata {
			if item.Metadata[key] != value {
				owned = false
				break
			}
		}
		if !owned {
			continue
		}
		if err := r.Recovery.DeleteSandbox(ctx, item.ID); err != nil && !errors.Is(err, canaryapi.ErrNotFound) {
			errs = append(errs, fmt.Errorf("delete recovered sandbox %s: %w", item.ID, err))
		} else {
			log.Info().Str("sandbox_id", item.ID).Msg("recovered run sandbox deleted through API")
		}
	}
	return errors.Join(errs...)
}
