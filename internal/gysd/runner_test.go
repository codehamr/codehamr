package gysd

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunCommandGreen exercises the real subprocess path: a trivially
// passing command must return ExitCode=0 with the expected output and no
// canceled/timeout flags. Without this, the gysd state machine tests
// would all be testing in isolation from the runner that produces them.
func TestRunCommandGreen(t *testing.T) {
	r := RunCommand(context.Background(), "echo HAMRER", 5*time.Second)
	if r.ExitCode != 0 {
		t.Fatalf("ExitCode=%d, want 0", r.ExitCode)
	}
	if r.Canceled || r.TimedOut {
		t.Fatalf("flags set unexpectedly: %+v", r)
	}
	if !strings.Contains(r.Output, "HAMRER") {
		t.Fatalf("Output missing echo: %q", r.Output)
	}
}

func TestRunCommandRed(t *testing.T) {
	r := RunCommand(context.Background(), "false", 5*time.Second)
	if r.ExitCode != 1 {
		t.Fatalf("ExitCode=%d, want 1", r.ExitCode)
	}
	if r.Canceled || r.TimedOut {
		t.Fatalf("flags set unexpectedly: %+v", r)
	}
}

func TestRunCommandTimeout(t *testing.T) {
	start := time.Now()
	// Sleep longer than the timeout should trigger TimedOut + ExitCode=124.
	r := RunCommand(context.Background(), "sleep 30", 200*time.Millisecond)
	elapsed := time.Since(start)
	if !r.TimedOut {
		t.Fatalf("TimedOut not set: %+v", r)
	}
	if r.ExitCode != 124 {
		t.Fatalf("ExitCode=%d, want 124 (POSIX timeout)", r.ExitCode)
	}
	if !strings.Contains(r.Output, "timeout after") {
		t.Fatalf("output missing timeout marker: %q", r.Output)
	}
	// Sanity-check the timeout actually fired in well under the 30s sleep —
	// process-group kill should land within a couple seconds tops.
	if elapsed > 3*time.Second {
		t.Fatalf("elapsed=%s — kill didn't fire promptly", elapsed)
	}
}

func TestRunCommandCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a brief moment — RunCommand should report Canceled,
	// not TimedOut, with the parent context's signal taking priority.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	r := RunCommand(ctx, "sleep 30", 30*time.Second)
	if !r.Canceled {
		t.Fatalf("Canceled not set: %+v", r)
	}
	if r.TimedOut {
		t.Fatal("TimedOut should not be set when parent canceled wins")
	}
}

func TestRunCommandKillsChildProcessGroup(t *testing.T) {
	// Backgrounded sleep is the canonical leak case: without process-group
	// kill, `sleep 30 &` survives parent shell exit and the timeout fires
	// only on the wait, leaving the child alive. Setpgid + kill(-pid) in
	// RunCommand should prevent that.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	RunCommand(ctx, "sleep 30 & sleep 30 & wait", 30*time.Second)
	elapsed := time.Since(start)
	// Without process-group kill, this would block until the WaitDelay or
	// the full sleep finishes. With it, the cancel terminates everything.
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed=%s — child process group not killed promptly", elapsed)
	}
}

func TestRunCommandCombinesStdoutStderr(t *testing.T) {
	r := RunCommand(context.Background(),
		"echo OUT && echo ERR >&2", 5*time.Second)
	if r.ExitCode != 0 {
		t.Fatalf("ExitCode=%d, want 0", r.ExitCode)
	}
	if !strings.Contains(r.Output, "OUT") || !strings.Contains(r.Output, "ERR") {
		t.Fatalf("Output missing stdout or stderr: %q", r.Output)
	}
}

func TestRunCommandANSIPassthroughForCaller(t *testing.T) {
	// RunCommand returns raw output; ANSI stripping is applied later in
	// Session.RecordVerify. Verify the runner doesn't mangle ANSI so the
	// downstream stripping has the right input.
	r := RunCommand(context.Background(),
		"printf '\\033[31mRED\\033[0m'", 5*time.Second)
	if !strings.Contains(r.Output, "\x1b[31m") {
		t.Fatalf("ANSI stripped at the runner layer: %q", r.Output)
	}
}
