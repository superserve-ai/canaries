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
	var staleCount int64
	for _, item := range items {
		expiresAt, err := time.Parse(time.RFC3339, item.Metadata["expires_at"])
		if err != nil || expiresAt.After(r.Clock().Add(-r.Config.JanitorThreshold)) {
			continue
		}
		staleCount++
		if err := r.Client.DeleteSandbox(ctx, item.ID); err != nil && err != canaryapi.ErrNotFound {
			log.Error().Err(err).Str("sandbox_id", item.ID).Msg("janitor delete failed")
		}
	}
	r.Metrics.RecordOrphans(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, staleCount)
	r.Metrics.RecordRun(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "janitor", "success", r.Clock().Sub(start))
	return nil
}
