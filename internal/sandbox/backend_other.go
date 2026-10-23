//go:build !darwin

package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
)

func detect() (Backend, bool) { return Backend{Reason: "no sandbox on " + runtime.GOOS}, true }

func confine(context.Context, policy) (*exec.Cmd, error) {
	return nil, errors.New("sandbox: no backend on " + runtime.GOOS)
}

// DenialHint is how to see what the sandbox refused; nothing is confined here.
const DenialHint = "nothing is confined on " + runtime.GOOS
