//go:build windows

package lock

import (
	"context"
	"fmt"
	"os"
	"time"
)

func acquireFileLock(_ context.Context, _ string, _ string, _ time.Duration) (Outcome, Lease, error) {
	return "", nil, fmt.Errorf("file locks are unsupported on windows in this build")
}

func unlockFile(_ *os.File) error {
	return nil
}
