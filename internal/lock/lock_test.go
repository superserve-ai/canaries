package lock

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	t.Run("none backend selected correctly", func(t *testing.T) {
		locker, _, err := New(context.Background(), Config{Backend: "none"})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := locker.(NoopLock); !ok {
			t.Fatalf("locker type = %T, want NoopLock", locker)
		}
	})

	t.Run("file backend selected correctly", func(t *testing.T) {
		locker, _, err := New(context.Background(), Config{Backend: "file", FilePath: filepath.Join(t.TempDir(), "canary.lock")})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := locker.(*FileLock); !ok {
			t.Fatalf("locker type = %T, want *FileLock", locker)
		}
	})

	t.Run("gcs backend selected correctly", func(t *testing.T) {
		locker, _, err := New(context.Background(), Config{Backend: "gcs", Bucket: "test-lock-bucket"})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := locker.(*GCSLock); !ok {
			t.Fatalf("locker type = %T, want *GCSLock", locker)
		}
	})

	t.Run("gcs auth error remains an error", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
		locker, _, err := New(context.Background(), Config{Backend: "gcs", Bucket: "test-lock-bucket"})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = locker.Acquire(context.Background(), "target", time.Minute)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
