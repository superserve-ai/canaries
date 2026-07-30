package lifecycle

import (
	"context"
	"embed"
	"fmt"
	"path/filepath"
)

//go:embed verification-utilities/*
var verificationUtilitiesFS embed.FS

func (r Runner) uploadVerificationUtilities(ctx context.Context, sandboxID, accessToken string) error {
	files := []string{
		"verification-utilities/verify_disk.sh",
		"verification-utilities/verify_memory.py",
	}
	start := r.Clock()
	var err error
	for _, name := range files {
		content, err := verificationUtilitiesFS.ReadFile(name)
		if err != nil {
			err = fmt.Errorf("read verification utility %s: %w", name, err)
			break
		}
		target := filepath.Join("/tmp/verification-utilities", filepath.Base(name))
		if writeErr := r.writeSandboxFileWithRetry(ctx, sandboxID, accessToken, target, content); writeErr != nil {
			err = fmt.Errorf("write verification utility %s: %w", name, writeErr)
			break
		}
	}
	r.Metrics.RecordStep(ctx, r.Config.Environment, r.Config.Region, r.Config.Target, "lifecycle", "prepare_verification_utilities", result(err), r.Clock().Sub(start))
	if err != nil {
		return fmt.Errorf("preparing verification utilities: %w", err)
	}
	return nil
}
