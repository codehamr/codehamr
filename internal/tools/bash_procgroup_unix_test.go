//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBashKillsBackgroundedChildOnCancel re-creates the coverage that lived in
// the deleted gysd/runner_test.go, now that bash owns the process-group kill
// (setProcessGroup in bash_unix.go). A naked `cmd &` backgrounds a grandchild
// that outlives /bin/sh; without the Setpgid + negative-PID SIGKILL on cancel,
// that grandchild leaks and runs to completion after the user has Ctrl+C'd.
// The test launches a long sleep in the background, captures its PID via a
// file, cancels the parent context, then polls until syscall.Kill(pid, 0)
// reports the process is gone (ESRCH). Deterministic and fast — it polls a
// deadline rather than sleeping the full 30s.
func TestBashKillsBackgroundedChildOnCancel(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	parent, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// `sleep 30 &` backgrounds the sleep; we record its PID and then `wait`
		// so /bin/sh stays alive holding the group together until cancel kills
		// the whole group.
		Bash(parent, "sleep 30 & echo $! > "+pidFile+"; wait", 60*time.Second)
		close(done)
	}()

	// Wait for the PID file to materialise (the child has started).
	pid := waitForPID(t, pidFile)

	// Sanity: the child is alive before we cancel — signal 0 just probes.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("backgrounded child %d should be alive before cancel: %v", pid, err)
	}

	// User Ctrl+C: cancel the parent context. setProcessGroup's Cancel hook
	// should SIGKILL the whole group, taking the backgrounded sleep with it.
	cancel()

	// Bash should return promptly once the group is killed.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Bash did not return within 10s after cancel — group kill failed")
	}

	// Poll until the child is reaped/gone. The kernel may take a beat to tear
	// the group down, so poll a deadline rather than asserting once.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return // child is gone — the group kill worked
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backgrounded child %d survived parent cancel — process group was not killed", pid)
}

// waitForPID blocks until pidFile contains a parseable PID, then returns it.
// Fails the test if the file never appears within a short deadline.
func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(raw))); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child PID file %s never appeared", pidFile)
	return 0
}
