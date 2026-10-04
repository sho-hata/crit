package lsp

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// DebugEnv turns on LSP debug logging into the daemon log
// (~/.crit/sessions/<key>.log). The daemon inherits it from the CLI, so a
// running daemon must be restarted to pick it up.
const DebugEnv = "CRIT_LSP_DEBUG"

// maxDebugPayload caps how much of one JSON-RPC frame or stderr line is
// logged. A didOpen carries the whole file and a publishDiagnostics can list
// hundreds of entries; the head of the frame is what identifies it.
const maxDebugPayload = 1024

// debugEnabled reads the environment on every call so tests can flip it with
// t.Setenv.
func debugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DebugEnv))) {
	case "", "0", "false", "off", "no":
		return false
	}
	return true
}

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

// truncateForLog escapes newlines so one frame stays on one log line.
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

// stderrLogger logs a language server's stderr line by line. os/exec drives
// it from a single copy goroutine, so it needs no locking.
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
