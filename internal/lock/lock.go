package lock

import (
	"context"
	"fmt"
	"time"
)

type Outcome string

const (
	OutcomeAcquired       Outcome = "acquired"
	OutcomeAlreadyRunning Outcome = "already_running"
)

type Lease interface {
	Release(ctx context.Context) error
}

type Lock interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (Outcome, Lease, error)
}

type Config struct {
	Backend  string
	FilePath string
	Bucket   string
}

type NoopLock struct{}

type noopLease struct{}

func (NoopLock) Acquire(_ context.Context, _ string, _ time.Duration) (Outcome, Lease, error) {
	return OutcomeAcquired, noopLease{}, nil
}

func (noopLease) Release(context.Context) error { return nil }

func New(ctx context.Context, cfg Config) (Lock, func(), error) {
	switch cfg.Backend {
	case "", "none":
		return NoopLock{}, func() {}, nil
	case "file":
		if cfg.FilePath == "" {
			return nil, nil, fmt.Errorf("CANARY_LOCK_FILE is required when CANARY_LOCK_BACKEND=file")
		}
		return NewFileLock(cfg.FilePath), func() {}, nil
	case "gcs":
		if cfg.Bucket == "" {
			return nil, nil, fmt.Errorf("LOCK_BUCKET is required when CANARY_LOCK_BACKEND=gcs")
		}
		locker := NewGCSLock(cfg.Bucket)
		return locker, func() { _ = locker.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unsupported lock backend %q", cfg.Backend)
	}
}
