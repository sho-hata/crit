// Package timing is a temporary probe for comparing E2E latency across OSes.
// With CRIT_TIMING_DIR set, every HTTP request and git invocation appends one
// line to <dir>/<pid>.log: unix-ns start, kind, name, duration in µs.
package timing

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	dir  = os.Getenv("CRIT_TIMING_DIR")
	once sync.Once
	mu   sync.Mutex
	out  *os.File
)

func Enabled() bool { return dir != "" }

func Log(kind, name string, start time.Time) {
	if dir == "" {
		return
	}
	d := time.Since(start)
	once.Do(func() {
		_ = os.MkdirAll(dir, 0o755)
		out, _ = os.OpenFile(filepath.Join(dir, fmt.Sprintf("%d.log", os.Getpid())), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	})
	if out == nil {
		return
	}
	mu.Lock()
	fmt.Fprintf(out, "%d\t%s\t%s\t%d\n", start.UnixNano(), kind, name, d.Microseconds())
	mu.Unlock()
}

// Cmd times Run, Output and CombinedOutput of the wrapped command.
type Cmd struct {
	*exec.Cmd
	label string
}

func Command(name string, args ...string) *Cmd {
	return &Cmd{Cmd: exec.Command(name, args...), label: label(name, args)}
}

func CommandContext(ctx context.Context, name string, args ...string) *Cmd {
	return &Cmd{Cmd: exec.CommandContext(ctx, name, args...), label: label(name, args)}
}

// label is the command plus its first non-flag argument (the git subcommand).
func label(name string, args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-C" || a == "-c" {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			return name + " " + a
		}
	}
	return name
}

func (c *Cmd) Run() error {
	defer Log("exec", c.label, time.Now())
	return c.Cmd.Run()
}

func (c *Cmd) Output() ([]byte, error) {
	defer Log("exec", c.label, time.Now())
	return c.Cmd.Output()
}

func (c *Cmd) CombinedOutput() ([]byte, error) {
	defer Log("exec", c.label, time.Now())
	return c.Cmd.CombinedOutput()
}
