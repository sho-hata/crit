package lsp

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
)

// logCapture redirects the standard logger into a buffer for one test. The
// logger is process-global, so tests using it must not call t.Parallel.
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLog(t *testing.T) *logCapture {
	t.Helper()
	lc := &logCapture{}
	flags := log.Flags()
	log.SetFlags(0)
	log.SetOutput(lc)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return lc
}

func TestDebugEnabled(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"off", false},
		{"no", false},
		{"  ", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"anything", true},
	}
	for _, tc := range cases {
		t.Run("value_"+tc.value, func(t *testing.T) {
			t.Setenv(DebugEnv, tc.value)
			if got := debugEnabled(); got != tc.want {
				t.Errorf("debugEnabled() with %s=%q = %v, want %v", DebugEnv, tc.value, got, tc.want)
			}
		})
	}
}

func TestTruncateForLog(t *testing.T) {
	t.Parallel()

	if got := truncateForLog([]byte("a\nb\r\nc")); got != `a\nb\r\nc` {
		t.Errorf("newlines not escaped: %q", got)
	}

	long := strings.Repeat("x", maxDebugPayload+37)
	got := truncateForLog([]byte(long))
	if !strings.HasPrefix(got, strings.Repeat("x", maxDebugPayload)) {
		t.Errorf("payload head was not kept")
	}
	if !strings.HasSuffix(got, "(+37 bytes)") {
		t.Errorf("truncation not reported: ...%q", got[len(got)-20:])
	}
}

func TestStderrLogger(t *testing.T) {
	lc := captureLog(t)

	w := &stderrLogger{name: "go"}
	// A line split across writes is held until its newline arrives; blank
	// lines are dropped; CRLF endings are trimmed.
	for _, chunk := range []string{"first li", "ne\nsecond\r\n\n", "third (no newline yet)"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}

	want := "lsp[go] stderr: first line\nlsp[go] stderr: second\n"
	if got := lc.String(); got != want {
		t.Errorf("log output:\n got %q\nwant %q", got, want)
	}
}

func TestStderrLoggerFlushesRunawayLine(t *testing.T) {
	lc := captureLog(t)

	w := &stderrLogger{name: "ts"}
	if _, err := w.Write([]byte(strings.Repeat("y", 5*maxDebugPayload))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lc.String(), "lsp[ts] stderr: yyy") {
		t.Errorf("an unterminated line past the buffer cap was not flushed: %q", lc.String())
	}
	if len(w.buf) != 0 {
		t.Errorf("buffer not reset after flush: %d bytes left", len(w.buf))
	}
}

func TestClientDebugLoggingOn(t *testing.T) {
	t.Setenv(DebugEnv, "1")
	lc := captureLog(t)

	fs := startFake(func(method string, params json.RawMessage) any {
		if method == "textDocument/hover" {
			return map[string]any{"contents": "docs"}
		}
		return nil
	})
	t.Cleanup(func() { fs.kill(); waitFor(t, "reader to stop", fs.client.Dead) })

	if _, err := fs.client.Hover("/tmp/x.go", 3, 4); err != nil {
		t.Fatal(err)
	}

	out := lc.String()
	for _, want := range []string{
		`lsp -> {"id":1,"jsonrpc":"2.0","method":"textDocument/hover"`, // request frame
		`lsp <- {"id":1,"jsonrpc":"2.0","result":{"contents":"docs"}}`, // response frame
		"lsp textDocument/hover #1 finished in ",                       // summary ties id to method
		": ok\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("debug log is missing %q\nfull log:\n%s", want, out)
		}
	}
}

func TestClientDebugLoggingReportsFailure(t *testing.T) {
	t.Setenv(DebugEnv, "1")
	lc := captureLog(t)

	fs := startFake(nil)
	t.Cleanup(func() { fs.kill(); waitFor(t, "reader to stop", fs.client.Dead) })
	fs.kill() // the transport dies before the request

	if _, err := fs.client.Hover("/tmp/x.go", 0, 0); err == nil {
		t.Fatal("expected an error from a dead transport")
	}
	waitFor(t, "failure summary", func() bool {
		return strings.Contains(lc.String(), "textDocument/hover #1 finished in ")
	})
	if out := lc.String(); !strings.Contains(out, "error: ") {
		t.Errorf("failed request not reported as an error:\n%s", out)
	}
}

func TestClientDebugLoggingOff(t *testing.T) {
	t.Setenv(DebugEnv, "")
	lc := captureLog(t)

	fs := startFake(func(method string, params json.RawMessage) any {
		if method == "textDocument/hover" {
			return map[string]any{"contents": "docs"}
		}
		return nil
	})

	if _, err := fs.client.Hover("/tmp/x.go", 3, 4); err != nil {
		t.Fatal(err)
	}
	fs.kill()
	waitFor(t, "client to notice the dead transport", fs.client.Dead)

	if out := lc.String(); out != "" {
		t.Errorf("nothing may be logged without %s, got:\n%s", DebugEnv, out)
	}
}
