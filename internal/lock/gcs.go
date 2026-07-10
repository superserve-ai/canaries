package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
)

type GCSLocker struct {
	client *storage.Client
	bucket string
}

type lockFile struct {
	Token      string    `json:"token"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Target     string    `json:"target"`
	Holder     string    `json:"holder"`
}

func NewGCSLocker(client *storage.Client, bucket string) *GCSLocker {
	return &GCSLocker{client: client, bucket: bucket}
}

func (l *GCSLocker) Acquire(ctx context.Context, key string, ttl time.Duration) (Result, error) {
	token := uuid.NewString()
	object := l.client.Bucket(l.bucket).Object("locks/" + key + ".json")
	now := time.Now().UTC()
	payload := lockFile{
		Token:      token,
		AcquiredAt: now,
		ExpiresAt:  now.Add(ttl),
		Target:     key,
		Holder:     "api-canary",
	}

	attrs, err := object.Attrs(ctx)
	switch {
	case err == nil:
		current, generation, err := l.readExisting(ctx, object, attrs.Generation)
		if err != nil {
			return Result{}, err
		}
		if current.ExpiresAt.After(now) {
			return Result{Skipped: true, ExpiresAt: current.ExpiresAt}, nil
		}
		if err := l.write(ctx, object.If(storage.Conditions{GenerationMatch: generation}), payload); err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				return Result{Skipped: true}, nil
			}
			return Result{Skipped: true}, nil
		}
	case errors.Is(err, storage.ErrObjectNotExist):
		if err := l.write(ctx, object.If(storage.Conditions{DoesNotExist: true}), payload); err != nil {
			return Result{Skipped: true}, nil
		}
	default:
		return Result{}, err
	}

	return Result{
		Acquired:   true,
		LeaseToken: token,
		ExpiresAt:  payload.ExpiresAt,
	}, nil
}

func (l *GCSLocker) Release(ctx context.Context, key, token string) error {
	object := l.client.Bucket(l.bucket).Object("locks/" + key + ".json")
	reader, err := object.NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil
		}
		return err
	}
	defer reader.Close()
	var current lockFile
	if err := json.NewDecoder(reader).Decode(&current); err != nil {
		return err
	}
	if current.Token != token {
		return nil
	}
	return object.Delete(ctx)
}

func (l *GCSLocker) readExisting(ctx context.Context, object *storage.ObjectHandle, generation int64) (lockFile, int64, error) {
	reader, err := object.Generation(generation).NewReader(ctx)
	if err != nil {
		return lockFile{}, 0, err
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		return lockFile{}, 0, err
	}
	var current lockFile
	if err := json.Unmarshal(body, &current); err != nil {
		return lockFile{}, 0, fmt.Errorf("decode lock: %w", err)
	}
	return current, generation, nil
}

func (l *GCSLocker) write(ctx context.Context, object *storage.ObjectHandle, payload lockFile) error {
	writer := object.NewWriter(ctx)
	writer.ContentType = "application/json"
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}
