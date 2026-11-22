package lsp

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

// WNOWAIT preserves the leader's PID until killTree has signalled the group.
func waitProcessExit(cmd *exec.Cmd) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
