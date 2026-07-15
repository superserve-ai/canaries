//go:build !windows

package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func acquireFileLock(_ context.Context, path, key string, _ time.Duration) (Outcome, Lease, error) {
	if err := ensureParentDir(path); err != nil {
		return "", nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return OutcomeAlreadyRunning, nil, nil
		}
		return "", nil, fmt.Errorf("acquire file lock: %w", err)
	}
	if err := writeLockMetadata(file, key); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return "", nil, err
	}
	return OutcomeAcquired, fileLease{file: file, path: path}, nil
}

func unlockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
