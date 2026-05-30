//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the shell in its own process group via Setpgid and
// installs a Cancel that kills the whole group with SIGKILL. Without this,
// backgrounded children (`cmd &`) survive parent shell exit on cancel or
// timeout — exactly the leak we set out to prevent. Salvaged from the deleted
// gysd/runner_unix.go when the verify tool was removed and the kill semantics
// folded into the one remaining shell tool. Unix-only because
// SysProcAttr.Setpgid and syscall.Kill negative-PID are not portable;
// Windows uses bash_windows.go.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid = the whole process group (Setpgid above made the
		// shell its own group leader).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
