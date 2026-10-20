package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

// sandboxExec is run by absolute path: a PATH lookup could find a planted one.
const sandboxExec = "/usr/bin/sandbox-exec"

// detect runs /usr/bin/true under the real profile shape, so a macOS release
// that rejects the profile text shows up here, not at the first hover. A
// missing sandbox-exec is final; a failed run is tried again next time.
func detect() (Backend, bool) {
	if _, err := os.Stat(sandboxExec); err != nil {
		return Backend{Reason: err.Error()}, true
	}
	pol := policy{
		readAllow: []string{"/nonexistent"},
		execDeny:  []string{"/nonexistent"},
		scratch:   os.TempDir(),
		argv:      []string{"/usr/bin/true"},
	}
	cmd, err := confine(context.Background(), pol)
	if err != nil {
		return Backend{Reason: err.Error()}, false
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return Backend{Reason: fmt.Sprintf("%s self-test failed: %v: %s", sandboxExec, err, bytes.TrimSpace(out))}, false
	}
	return Backend{Name: "seatbelt"}, true
}

// confine wraps argv in sandbox-exec, which applies the profile and then
// execs the program in place: the pid is the server's, not a wrapper's.
func confine(ctx context.Context, pol policy) (*exec.Cmd, error) {
	text, params := profile(pol)
	args := []string{"-p", text}
	for _, kv := range params {
		args = append(args, "-D", kv)
	}
	args = append(args, "--")
	args = append(args, pol.argv...)
	return exec.CommandContext(ctx, sandboxExec, args...), nil
}

// DenialHint is how to see what the sandbox refused: seatbelt reports to the
// unified log, under this sender, and only live (never in `log show`).
const DenialHint = `/usr/bin/log stream --predicate 'sender == "Sandbox"'`
