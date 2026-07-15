package lock

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileLockHelperProcess(t *testing.T) {
	if os.Getenv("TEST_FILE_LOCK_HELPER") != "1" {
		return
	}
	path := os.Getenv("TEST_FILE_LOCK_PATH")
	target := os.Getenv("TEST_FILE_LOCK_TARGET")
	action := os.Getenv("TEST_FILE_LOCK_ACTION")

	locker := NewFileLock(path)
	outcome, lease, err := locker.Acquire(context.Background(), target, time.Minute)
	if err != nil {
		fmt.Fprintln(os.Stdout, "error:", err)
		os.Exit(2)
	}
	if outcome == OutcomeAlreadyRunning {
		fmt.Fprintln(os.Stdout, "already_running")
		os.Exit(0)
	}
	fmt.Fprintln(os.Stdout, "acquired")

	switch action {
	case "release":
		if err := lease.Release(context.Background()); err != nil {
			fmt.Fprintln(os.Stdout, "error:", err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stdout, "released")
		os.Exit(0)
	default:
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

func TestFileLockProcessBehavior(t *testing.T) {
	t.Run("first process acquires lock", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "same.lock")
		target := "staging-us-central1"
		first := startFileLockHelper(t, path, target, "hold")
		if got := waitForHelperLine(t, first); got != "acquired" {
			t.Fatalf("first process = %q", got)
		}
		second := startFileLockHelper(t, path, target, "hold")
		if got := waitForHelperLine(t, second); got != "already_running" {
			t.Fatalf("second process = %q", got)
		}
		stopFileLockHelper(t, first)
		stopFileLockHelper(t, second)
	})

	t.Run("lock becomes available after first process exits", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "same.lock")
		target := "staging-us-central1"
		first := startFileLockHelper(t, path, target, "hold")
		if got := waitForHelperLine(t, first); got != "acquired" {
			t.Fatalf("first process = %q", got)
		}
		stopFileLockHelper(t, first)
		second := startFileLockHelper(t, path, target, "hold")
		if got := waitForHelperLine(t, second); got != "acquired" {
			t.Fatalf("second process = %q", got)
		}
		stopFileLockHelper(t, second)
	})

	t.Run("lock becomes available after explicit release", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "same.lock")
		target := "staging-us-central1"
		first := startFileLockHelper(t, path, target, "release")
		if got := waitForHelperLine(t, first); got != "acquired" {
			t.Fatalf("first process = %q", got)
		}
		if got := waitForHelperLine(t, first); got != "released" {
			t.Fatalf("first release = %q", got)
		}
		stopFileLockHelper(t, first)
		second := startFileLockHelper(t, path, target, "hold")
		if got := waitForHelperLine(t, second); got != "acquired" {
			t.Fatalf("second process = %q", got)
		}
		stopFileLockHelper(t, second)
	})

	t.Run("different targets may be held concurrently", func(t *testing.T) {
		dir := t.TempDir()
		firstTarget := "staging-us-central1"
		secondTarget := "production-us-west2"
		first := startFileLockHelper(t, filepath.Join(dir, "superserve-canary-"+firstTarget+".lock"), firstTarget, "hold")
		if got := waitForHelperLine(t, first); got != "acquired" {
			t.Fatalf("first process = %q", got)
		}
		second := startFileLockHelper(t, filepath.Join(dir, "superserve-canary-"+secondTarget+".lock"), secondTarget, "hold")
		if got := waitForHelperLine(t, second); got != "acquired" {
			t.Fatalf("second process = %q", got)
		}
		stopFileLockHelper(t, first)
		stopFileLockHelper(t, second)
	})

	t.Run("custom lock path works", func(t *testing.T) {
		customPath := filepath.Join(t.TempDir(), "custom", "canary.lock")
		target := "staging-us-central1"
		proc := startFileLockHelper(t, customPath, target, "release")
		if got := waitForHelperLine(t, proc); got != "acquired" {
			t.Fatalf("process = %q", got)
		}
		if got := waitForHelperLine(t, proc); got != "released" {
			t.Fatalf("process = %q", got)
		}
		stopFileLockHelper(t, proc)
		if _, err := os.Stat(customPath); err != nil {
			t.Fatalf("stat custom path: %v", err)
		}
	})

	t.Run("parent directory is created", func(t *testing.T) {
		customPath := filepath.Join(t.TempDir(), "nested", "locks", "canary.lock")
		target := "staging-us-central1"
		proc := startFileLockHelper(t, customPath, target, "release")
		if got := waitForHelperLine(t, proc); got != "acquired" {
			t.Fatalf("process = %q", got)
		}
		if got := waitForHelperLine(t, proc); got != "released" {
			t.Fatalf("process = %q", got)
		}
		stopFileLockHelper(t, proc)
		if _, err := os.Stat(filepath.Dir(customPath)); err != nil {
			t.Fatalf("stat parent dir: %v", err)
		}
	})
}

type helperProcess struct {
	cmd    *exec.Cmd
	stdin  io.Closer
	stdout io.ReadCloser
	lines  chan string
}

func startFileLockHelper(t *testing.T, path, target, action string) helperProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestFileLockHelperProcess")
	cmd.Env = append(os.Environ(),
		"TEST_FILE_LOCK_HELPER=1",
		"TEST_FILE_LOCK_PATH="+path,
		"TEST_FILE_LOCK_TARGET="+target,
		"TEST_FILE_LOCK_ACTION="+action,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- strings.TrimSpace(scanner.Text())
		}
		close(lines)
	}()
	return helperProcess{cmd: cmd, stdin: stdin, stdout: stdout, lines: lines}
}

func waitForHelperLine(t *testing.T, proc helperProcess) string {
	t.Helper()
	select {
	case line, ok := <-proc.lines:
		if !ok {
			return ""
		}
		return line
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for helper output")
		return ""
	}
}

func stopFileLockHelper(t *testing.T, proc helperProcess) {
	t.Helper()
	if proc.stdin != nil {
		_ = proc.stdin.Close()
	}
	if proc.cmd != nil {
		if err := proc.cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if proc.stdout != nil {
		_ = proc.stdout.Close()
	}
}
