//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package lsp

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

// NOTE_EXIT observes termination without reaping the process. Registration
// against a process that has already exited may return ESRCH; its unreaped
// PID is still reserved, so group cleanup remains safe.
func waitProcessExit(cmd *exec.Cmd) error {
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	unix.CloseOnExec(fd)
	var event unix.Kevent_t
	unix.SetKevent(&event, cmd.Process.Pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	event.Fflags = unix.NOTE_EXIT
	for {
		_, err = unix.Kevent(fd, []unix.Kevent_t{event}, nil, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	events := make([]unix.Kevent_t, 1)
	for {
		_, err = unix.Kevent(fd, nil, events, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
