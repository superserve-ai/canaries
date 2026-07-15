package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/googleapi"
)

type GCSLock struct {
	mu     sync.Mutex
	client *storage.Client
	bucket string
}

type gcsLease struct {
	client *storage.Client
	bucket string
	key    string
	token  string
}

type lockFile struct {
	Token      string    `json:"token"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Target     string    `json:"target"`
	Holder     string    `json:"holder"`
}

func NewGCSLock(bucket string) *GCSLock {
	return &GCSLock{bucket: bucket}
}

func (l *GCSLock) Acquire(ctx context.Context, key string, ttl time.Duration) (Outcome, Lease, error) {
	if err := l.ensureClient(ctx); err != nil {
		return "", nil, err
	}
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
			return "", nil, err
		}
		if current.ExpiresAt.After(now) {
			return OutcomeAlreadyRunning, nil, nil
		}
		if err := l.write(ctx, object.If(storage.Conditions{GenerationMatch: generation}), payload); err != nil {
			if isAlreadyRunningWriteError(err) {
				return OutcomeAlreadyRunning, nil, nil
			}
			return "", nil, err
		}
	case errors.Is(err, storage.ErrObjectNotExist):
		if err := l.write(ctx, object.If(storage.Conditions{DoesNotExist: true}), payload); err != nil {
			if isAlreadyRunningWriteError(err) {
				return OutcomeAlreadyRunning, nil, nil
			}
			return "", nil, err
		}
	default:
		return "", nil, err
	}

	return OutcomeAcquired, gcsLease{
		client: l.client,
		bucket: l.bucket,
		key:    key,
		token:  token,
	}, nil
}

func isAlreadyRunningWriteError(err error) bool {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return true
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed {
		return true
	}
	return false
}

func (l *GCSLock) ensureClient(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client != nil {
		return nil
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create storage client: %w", err)
	}
	l.client = client
	return nil
}

func (l *GCSLock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client == nil {
		return nil
	}
	err := l.client.Close()
	l.client = nil
	return err
}

func (l *GCSLock) readExisting(ctx context.Context, object *storage.ObjectHandle, generation int64) (lockFile, int64, error) {
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

func (l *GCSLock) write(ctx context.Context, object *storage.ObjectHandle, payload lockFile) error {
	writer := object.NewWriter(ctx)
	writer.ContentType = "application/json"
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func (l *GCSLock) Release(ctx context.Context, key, token string) error {
	if l.client == nil {
		return nil
	}
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

func (l gcsLease) Release(ctx context.Context) error {
	if l.client == nil {
		return nil
	}
	object := l.client.Bucket(l.bucket).Object("locks/" + l.key + ".json")
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
	if current.Token != l.token {
		return nil
	}
	return object.Delete(ctx)
}
