package lsp

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// DebugEnv names the environment variable that turns on LSP debug logging.
// The daemon reads it at startup (it inherits the CLI's environment), so an
// already-running daemon has to be restarted to pick it up.
//
// When enabled, everything goes through the standard logger — i.e. into the
// daemon log at ~/.crit/sessions/<key>.log:
//   - each server's stderr, which is discarded otherwise
//   - every JSON-RPC frame in both directions, payloads truncated
//   - one summary line per request with its duration and outcome
//   - server lifecycle: spawn, handshake, exit, restart, idle shutdown
const DebugEnv = "CRIT_LSP_DEBUG"

// maxDebugPayload caps how much of one JSON-RPC frame or stderr line is
// logged. A didOpen carries the whole file and a publishDiagnostics can list
// hundreds of entries; the head of the frame is what identifies it.
const maxDebugPayload = 1024

// debugEnabled reports whether DebugEnv is set to something other than an
// explicit "off" value. It reads the environment on every call so tests can
// flip it with t.Setenv.
func debugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DebugEnv))) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

// debugf writes one debug line tagged with the language name, or nothing when
// debug logging is off. name is "" for clients that aren't tied to a
// registry language (in-memory test transports).
func debugf(enabled bool, name, format string, args ...any) {
	if !enabled {
		return
	}
	tag := "lsp"
	if name != "" {
		tag = "lsp[" + name + "]"
	}
	log.Printf("%s %s", tag, fmt.Sprintf(format, args...))
}

// roundDuration trims d for a log line: whole milliseconds once it reaches
// one, microseconds below that so a fast answer doesn't print as "0s".
func roundDuration(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return d.Round(time.Microsecond)
	}
	return d.Round(time.Millisecond)
}

// truncateForLog shortens b to maxDebugPayload bytes for a log line, noting
// how much was cut. Newlines are escaped so one frame stays on one line.
func truncateForLog(b []byte) string {
	cut := 0
	if len(b) > maxDebugPayload {
		cut = len(b) - maxDebugPayload
		b = b[:maxDebugPayload]
	}
	s := strings.ReplaceAll(string(b), "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	if cut > 0 {
		s += fmt.Sprintf("… (+%d bytes)", cut)
	}
	return s
}

// stderrLogger is an io.Writer that logs a language server's stderr one line
// at a time. os/exec drives it from a single copy goroutine, so it needs no
// locking.
type stderrLogger struct {
	name string
	buf  []byte
}

func (w *stderrLogger) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	// A server that never ends its line must not grow the buffer without
	// bound; flush what we have as if it were a line.
	if len(w.buf) > 4*maxDebugPayload {
		w.emit(w.buf)
		w.buf = nil
	}
	return len(p), nil
}

func (w *stderrLogger) emit(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return
	}
	debugf(true, w.name, "stderr: %s", truncateForLog(line))
}
