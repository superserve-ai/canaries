package janitor

import (
	"context"
	"errors"
	"strconv"
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
	ListTemplates(context.Context, map[string]string) ([]canaryapi.Template, error)
	DeleteTemplate(context.Context, string) error
	DeleteSnapshot(context.Context, string) error
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
		if err := r.Client.DeleteSandbox(ctx, item.ID); err != nil && !errors.Is(err, canaryapi.ErrNotFound) {
			deletionFailureCount++
			log.Error().Err(err).Str("sandbox_id", item.ID).Msg("janitor delete failed")
			if orphanAge > oldestOrphanAge {
				oldestOrphanAge = orphanAge
			}
			continue
		}
		deletedCount++
	}

	templates, err := r.Client.ListTemplates(ctx, map[string]string{
		"owner":       "team",
		"name_prefix": sandboxmetadata.TemplateNamePrefix,
		"limit":       strconv.Itoa(templateListLimit),
	})
	if err != nil {
		r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", "failure", r.Clock().Sub(start))
		return err
	}
	var templateDeletedCount int64
	for _, tpl := range templates {
		examinedCount++
		staleSince := tpl.CreatedAt.Add(r.templateTTL())
		if staleSince.After(now) {
			continue
		}
		staleCount++
		if err := r.Client.DeleteTemplate(ctx, tpl.ID); err != nil && !errors.Is(err, canaryapi.ErrNotFound) {
			deletionFailureCount++
			log.Error().Err(err).Str("template_id", tpl.ID).Msg("janitor template delete failed")
			if age := now.Sub(staleSince); age > oldestOrphanAge {
				oldestOrphanAge = age
			}
			continue
		}
		deletedCount++
		templateDeletedCount++
	}

	// Snapshots outlive their sandbox and have no list-all endpoint, so the
	// sweep reads the snapshot id the run recorded on its soft-deleted source.
	sources, err := r.Client.ListSandboxes(ctx, sandboxmetadata.DeletedSnapshotSourcesQuery(r.Config.Environment, templateListLimit))
	if err != nil {
		r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", "failure", r.Clock().Sub(start))
		return err
	}
	var snapshotDeletedCount int64
	for _, item := range sources {
		snapshotID := item.Metadata[sandboxmetadata.KeySnapshotID]
		match, ok := sandboxmetadata.MatchOwnership(item.Metadata, r.Config.Environment)
		if snapshotID == "" || !ok {
			continue
		}
		examinedCount++
		orphanAge, stale := sandboxmetadata.StaleSince(item.Metadata, match, now, r.Config.RetainFailedSandboxTTL)
		if !stale {
			continue
		}
		staleCount++
		if err := r.Client.DeleteSnapshot(ctx, snapshotID); err != nil && !errors.Is(err, canaryapi.ErrNotFound) {
			deletionFailureCount++
			log.Error().Err(err).Str("snapshot_id", snapshotID).Str("sandbox_id", item.ID).Msg("janitor snapshot delete failed")
			if orphanAge > oldestOrphanAge {
				oldestOrphanAge = orphanAge
			}
			continue
		}
		deletedCount++
		snapshotDeletedCount++
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
		Int64("templates_examined", int64(len(templates))).
		Int64("templates_deleted", templateDeletedCount).
		Int64("snapshots_deleted", snapshotDeletedCount).
		Msg("janitor retention sweep complete")
	r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", "success", r.Clock().Sub(start))
	return nil
}

// Team template and snapshot quotas are small, so one page covers a sweep.
const templateListLimit = 100

// A canary template outlives its run only when the run failed with retention
// on, so it is stale once the longer of the two TTLs has passed.
func (r Runner) templateTTL() time.Duration {
	if r.Config.RetainFailedSandboxTTL > r.Config.ResourceTTL {
		return r.Config.RetainFailedSandboxTTL
	}
	return r.Config.ResourceTTL
}
