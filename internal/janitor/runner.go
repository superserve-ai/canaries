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
	r.Metrics.RecordExecutionDelta(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", 1)
	defer r.Metrics.RecordExecutionDelta(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", -1)

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
	var deletedCount int64
	var deletionFailureCount int64
	var staleCount int64
	var oldestOrphanAge time.Duration
	for _, item := range items {
		if item.Metadata["managed_by"] != "api-canary" {
			continue
		}
		examinedCount++
		var orphanAge time.Duration
		expiresAt, expiresErr := time.Parse(time.RFC3339, item.Metadata["expires_at"])
		retained := item.Metadata["retained_for_debug"] == "true"
		if expiresErr == nil {
			if expiresAt.After(now) {
				continue
			}
			orphanAge = now.Sub(expiresAt)
		} else {
			if !retained {
				continue
			}
			createdAt, createdErr := time.Parse(time.RFC3339, item.Metadata["created_at"])
			if createdErr != nil {
				continue
			}
			orphanAge = now.Sub(createdAt.Add(r.Config.RetainFailedSandboxTTL))
			if orphanAge < 0 {
				continue
			}
		}
		staleCount++
		if err := r.Client.DeleteSandbox(ctx, item.ID); err != nil && err != canaryapi.ErrNotFound {
			deletionFailureCount++
			log.Error().Err(err).Str("sandbox_id", item.ID).Msg("janitor delete failed")
			if orphanAge > oldestOrphanAge {
				oldestOrphanAge = orphanAge
			}
			continue
		}
		deletedCount++
	}
	currentOrphanCount := staleCount - deletedCount
	if currentOrphanCount < 0 {
		currentOrphanCount = 0
	}
	if currentOrphanCount == 0 {
		oldestOrphanAge = 0
	}
	r.Metrics.RecordOrphans(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, currentOrphanCount, oldestOrphanAge)
	r.Metrics.RecordJanitorResources(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, examinedCount, deletedCount, deletionFailureCount)
	log.Info().
		Int64("retained_examined", examinedCount).
		Int64("retained_stale", staleCount).
		Int64("retained_deleted", deletedCount).
		Int64("retained_deletion_failures", deletionFailureCount).
		Msg("janitor retention sweep complete")
	r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", "success", r.Clock().Sub(start))
	return nil
}
