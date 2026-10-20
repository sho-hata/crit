//go:build windows

package lsp

import "os/exec"

func isolate(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
