//go:build windows

package lsp

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

// Windows has no process group to kill here: isolate is a no-op and
// killTree ends the leader alone, so a child a server forked can outlive
// Close there. A per-server job object (internal/daemon has the pattern)
// would close that gap.
func isolate(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) { _ = cmd.Process.Kill() }

func waitProcessExit(cmd *exec.Cmd) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	_, err = windows.WaitForSingleObject(handle, windows.INFINITE)
	return err
}
