package janitor

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/superserve-ai/canaries/internal/canaryapi"
	"github.com/superserve-ai/canaries/internal/config"
	"github.com/superserve-ai/canaries/internal/metrics"
	"github.com/superserve-ai/canaries/internal/sandboxmetadata"
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

	itemsByID := map[string]canaryapi.Sandbox{}
	for _, managedBy := range sandboxmetadata.RecognizedManagedByValues() {
		items, err := r.Client.ListSandboxes(ctx, sandboxmetadata.OwnershipQuery(r.Config.Environment, managedBy))
		if err != nil {
			r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", "failure", r.Clock().Sub(start))
			return err
		}
		for _, item := range items {
			if _, ok := itemsByID[item.ID]; ok {
				continue
			}
			itemsByID[item.ID] = item
		}
	}
	now := r.Clock().UTC()
	var examinedCount int64
	var deletedCount int64
	var deletionFailureCount int64
	var staleCount int64
	var oldestOrphanAge time.Duration
	for _, item := range itemsByID {
		match, ok := sandboxmetadata.MatchOwnership(item.Metadata, r.Config.Environment)
		if !ok {
			continue
		}
		examinedCount++
		orphanAge, stale := sandboxmetadata.StaleSince(item.Metadata, match, now, r.Config.RetainFailedSandboxTTL)
		if !stale {
			continue
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
