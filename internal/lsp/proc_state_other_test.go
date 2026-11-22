//go:build !windows && !linux && !darwin

package lsp

import (
	"errors"
	"syscall"
)

func processDead(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }
