//go:build !linux

package sandbox

// HideFromSameUser hides the process's memory and environment from other
// processes of the same user. Only Linux has the switch (see hide_linux.go).
func HideFromSameUser() {}
