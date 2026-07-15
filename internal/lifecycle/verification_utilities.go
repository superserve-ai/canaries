package lifecycle

import (
	"context"
	"embed"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
)

//go:embed verification-utilities/*
var verificationUtilitiesFS embed.FS

func (r Runner) uploadVerificationUtilities(ctx context.Context, sandboxID, accessToken string) error {
	files := []string{
		"verification-utilities/verify_disk.sh",
		"verification-utilities/verify_memory.py",
	}
	var commands []string
	commands = append(commands, "mkdir -p /tmp/verification-utilities")
	for _, name := range files {
		content, err := verificationUtilitiesFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read verification utility %s: %w", name, err)
		}
		target := filepath.Join("/tmp/verification-utilities", filepath.Base(name))
		commands = append(commands, fmt.Sprintf(
			"printf %%s %s | base64 -d > %s",
			shellQuote(base64.StdEncoding.EncodeToString(content)),
			shellQuote(target),
		))
		if strings.HasSuffix(name, ".sh") || strings.HasSuffix(name, ".py") {
			commands = append(commands, fmt.Sprintf("chmod 755 %s", shellQuote(target)))
		}
	}
	uploadCmd := "sh -lc " + shellQuote(strings.Join(commands, " && "))
	if _, err := r.execStep(ctx, sandboxID, accessToken, "prepare_verification_utilities", uploadCmd); err != nil {
		return fmt.Errorf("preparing verification utilities: %w", err)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
