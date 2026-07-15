package lock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type FileLock struct {
	path string
}

type fileLease struct {
	file *os.File
	path string
}

func NewFileLock(path string) *FileLock {
	return &FileLock{path: path}
}

func (l *FileLock) Acquire(ctx context.Context, key string, ttl time.Duration) (Outcome, Lease, error) {
	return acquireFileLock(ctx, l.path, key, ttl)
}

func (f fileLease) Release(context.Context) error {
	if f.file == nil {
		return nil
	}
	if err := unlockFile(f.file); err != nil {
		_ = f.file.Close()
		return err
	}
	return f.file.Close()
}

func writeLockMetadata(file *os.File, key string) error {
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	metadata := fmt.Sprintf("pid=%d\ntarget=%s\nstarted_at=%s\n", os.Getpid(), key, time.Now().UTC().Format(time.RFC3339))
	if _, err := file.WriteString(metadata); err != nil {
		return err
	}
	return file.Sync()
}

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "." || dir == string(filepath.Separator) {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

func sanitizeLockPath(path string) string {
	return strings.TrimSpace(path)
}
