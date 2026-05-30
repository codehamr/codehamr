//go:build windows

package tools

import "os/exec"

// setProcessGroup is a no-op on Windows. codehamr targets Linux/macOS dev
// containers; the Windows build exists so cross-compile in CI doesn't fail,
// but the bash tool relies on /bin/sh and POSIX process groups and is not
// expected to function on a native Windows host. Default exec.CommandContext
// cancellation (single-process kill) applies — adequate for a non-functional
// cross-compile target. Mirrors the deleted gysd/runner_windows.go.
func setProcessGroup(_ *exec.Cmd) {}
