package janitor

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/metrics"
)

type Runner struct {
	Config  config.Config
	Client  Client
	Metrics metrics.Provider
	Clock   func() time.Time
}

type Client interface {
	ListSandboxes(context.Context, map[string]string) ([]canaryapi.Sandbox, error)
	DeleteSandbox(context.Context, string) error
}

func (r Runner) Run(ctx context.Context) error {
	start := r.Clock()
	items, err := r.Client.ListSandboxes(ctx, map[string]string{
		"metadata.managed_by":  "api-canary",
		"metadata.environment": r.Config.Environment,
	})
	if err != nil {
		r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", "failure", r.Clock().Sub(start))
		return err
	}
	now := r.Clock().UTC()
	var examinedCount int64
	var expiredCount int64
	var deletedCount int64
	var deletionFailureCount int64
	for _, item := range items {
		if item.Metadata["managed_by"] != "api-canary" {
			continue
		}
		examinedCount++
		expiresAt, expiresErr := time.Parse(time.RFC3339, item.Metadata["expires_at"])
		retained := item.Metadata["retained_for_debug"] == "true"
		if expiresErr == nil {
			if expiresAt.After(now) {
				continue
			}
		} else {
			if !retained {
				continue
			}
			createdAt, createdErr := time.Parse(time.RFC3339, item.Metadata["created_at"])
			if createdErr != nil {
				continue
			}
			if createdAt.Add(r.Config.RetainFailedSandboxTTL).After(now) {
				continue
			}
		}
		expiredCount++
		if err := r.Client.DeleteSandbox(ctx, item.ID); err != nil && err != canaryapi.ErrNotFound {
			deletionFailureCount++
			log.Error().Err(err).Str("sandbox_id", item.ID).Msg("janitor delete failed")
			continue
		}
		deletedCount++
	}
	r.Metrics.RecordOrphans(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, deletedCount)
	r.Metrics.RecordRetainedSandboxJanitor(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, examinedCount, expiredCount, deletedCount, deletionFailureCount)
	log.Info().
		Int64("retained_examined", examinedCount).
		Int64("retained_expired", expiredCount).
		Int64("retained_deleted", deletedCount).
		Int64("retained_deletion_failures", deletionFailureCount).
		Msg("janitor retention sweep complete")
	r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", "success", r.Clock().Sub(start))
	return nil
}
