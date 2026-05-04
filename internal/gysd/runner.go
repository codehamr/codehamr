package gysd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// RunOutcome carries everything Session.RecordVerify needs from a finished
// verify subprocess. Output is the combined stdout+stderr; ExitCode reflects
// the shell's actual exit (0 = green); Canceled and TimedOut are set when
// the subprocess didn't get to finish on its own.
type RunOutcome struct {
	Output   string
	ExitCode int
	Canceled bool
	TimedOut bool
}

// RunCommand runs cmd via /bin/sh -c with the given timeout. On Unix the
// shell is placed in its own process group via setProcessGroup so cancel
// or timeout kills the whole tree (see runner_unix.go); on Windows the
// default exec.Cmd cancellation applies (see runner_windows.go) — Windows
// is a cross-compile target only, the verify path is not exercised there.
// The TUI calls this from a tea.Cmd goroutine and pipes the result back
// via verifyResultMsg — the gysd state machine stays single-goroutine.
func RunCommand(parent context.Context, command string, timeout time.Duration) RunOutcome {
	ctxT, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctxT, "/bin/sh", "-c", command)
	setProcessGroup(cmd)
	// Bound the wait for stdout/stderr pipes to close once the shell exits;
	// without this, leaked-fd children would keep CombinedOutput blocking
	// for the full timeout even after the kill.
	cmd.WaitDelay = 100 * time.Millisecond

	out, err := cmd.CombinedOutput()
	res := RunOutcome{Output: string(out)}

	if err == nil {
		return res
	}

	// Order matters: parent cancel beats timeout (user Ctrl+C wins clearly);
	// timeout beats exit error (the process didn't actually finish).
	if parent.Err() == context.Canceled {
		res.Canceled = true
		return res
	}
	if ctxT.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = 124 // matches POSIX `timeout` utility convention
		res.Output = res.Output + fmt.Sprintf("\n(timeout after %s)", timeout)
		return res
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res
	}
	// Exec setup failures (path resolution, permission) — surface as a
	// distinct exit code so the tool-result reads sensibly.
	res.ExitCode = -1
	res.Output = res.Output + fmt.Sprintf("\n(exec error: %v)", err)
	return res
}
