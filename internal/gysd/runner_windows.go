//go:build windows

package gysd

import "os/exec"

// setProcessGroup is a no-op on Windows. codehamr targets Linux/macOS dev
// containers; the Windows build exists so cross-compile in CI doesn't
// fail, but the verify subprocess relies on /bin/sh and POSIX process
// groups and is not expected to function on a native Windows host.
// Default exec.CommandContext cancellation (single-process SIGKILL) is
// applied — adequate for a non-functional cross-compile target.
func setProcessGroup(_ *exec.Cmd) {}
