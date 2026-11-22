//go:build !windows

package lsp

import (
	"os/exec"
	"syscall"
)

// isolate gives the server its own process group, so killTree reaches what
// it forks (typescript-language-server's tsserver) as well as itself.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
