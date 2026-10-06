package lifecycle

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Runs the real verifier the way the sandbox does: through a shell whose
// command line carries the token. It must fail with no sleeper and pass once
// a process outside its own ancestry holds the token.
func TestVerifyMemoryScriptIgnoresItsOwnCommandLine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	script := filepath.Join(t.TempDir(), "verify_memory.py")
	content, err := verificationUtilitiesFS.ReadFile("verification-utilities/verify_memory.py")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, content, 0o644); err != nil {
		t.Fatal(err)
	}
	token := "mem-" + filepath.Base(t.TempDir())
	run := func() error {
		cmd := exec.Command("sh", "-lc", "CANARY_MEMORY_TOKEN="+token+" "+python+" "+script)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return &execError{err: err, out: string(out)}
		}
		return nil
	}

	if err := run(); err == nil {
		t.Fatal("verifier passed with no sleeper; it matched its own command line")
	}

	sleeper := exec.Command(python, "-c", "import time; time.sleep(60)", token)
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()

	if err := run(); err != nil {
		t.Fatalf("verifier failed with the sleeper running: %v", err)
	}
}

type execError struct {
	err error
	out string
}

func (e *execError) Error() string { return e.err.Error() + ": " + e.out }
