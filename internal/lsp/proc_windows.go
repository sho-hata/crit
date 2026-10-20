//go:build windows

package lsp

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

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
