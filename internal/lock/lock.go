package lock

import (
	"context"
	"time"
)

type Result struct {
	Acquired   bool
	Skipped    bool
	LeaseToken string
	ExpiresAt  time.Time
}

type Locker interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (Result, error)
	Release(ctx context.Context, key, token string) error
}

type NoopLocker struct{}

func (NoopLocker) Acquire(_ context.Context, _ string, ttl time.Duration) (Result, error) {
	return Result{Acquired: true, LeaseToken: "noop", ExpiresAt: time.Now().Add(ttl)}, nil
}

func (NoopLocker) Release(context.Context, string, string) error { return nil }
