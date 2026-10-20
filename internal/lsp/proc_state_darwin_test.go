package lsp

import "golang.org/x/sys/unix"

// processDead reports a pid that is gone or a zombie (p_stat SZOMB).
func processDead(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return true
	}
	const szomb = 5 // sys/proc.h
	return kp.Proc.P_stat == szomb
}
