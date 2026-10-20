// Package sandboxtest plants a canary that tries, from inside a sandbox,
// everything the sandbox must prevent, and asserts the outcome. It is the
// only code allowed to run the canary unconfined (AssertEscaped), the control
// that proves the probes are live.
package sandboxtest

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Canary is one planted probe set bound to a listener on 127.0.0.1.
type Canary struct {
	tree     string
	script   string
	marker   string // a write target outside both tree and scratch
	secret   string // a file under the fake HOME's .ssh
	treeHash string
	port     int
	accepted atomic.Int32
}

// connect is TCP; udp proves the denial is of the socket, not of a TCP
// connect alone (a connectionless connect
// always succeeds unconfined). copy-exec copies the tree's binary into the
// scratch and runs it from there: the one writable place must not be an
// executable one.
var steps = []string{"outside-write", "tree-write", "connect", "udp", "exec", "copy-exec", "secret"}

// Plant writes canary.sh and an executable bin/canary into tree, points HOME
// at a fresh directory holding .ssh/id_canary (so call it before
// sandbox.Open), starts the listener, and records the tree's content hash.
func Plant(t testing.TB, tree string) *Canary {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	secret := filepath.Join(home, ".ssh", "id_canary")
	mustWrite(t, secret, "not a key\n", 0o600)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	c := &Canary{
		tree:   tree,
		script: filepath.Join(tree, "canary.sh"),
		marker: filepath.Join(t.TempDir(), "escaped"),
		secret: secret,
		port:   ln.Addr().(*net.TCPAddr).Port,
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.accepted.Add(1)
			conn.Close()
		}
	}()

	// A shebang script, not a copied binary: macOS kills copies of its own
	// signed binaries wherever they run.
	mustWrite(t, filepath.Join(tree, "bin", "canary"), "#!/bin/sh\n: > \"${TMPDIR:?}/exec-ran\"\n", 0o755)
	mustWrite(t, c.script, c.scriptText(), 0o644)
	c.treeHash = hashTree(t, tree)
	return c
}

// Each probe appends "<step> ok|denied" to $TMPDIR/probe.log; the jail sets
// TMPDIR under its scratch, the only place a confined write may land. The
// subshells keep a failed redirection from exiting a POSIX sh.
func (c *Canary) scriptText() string {
	return fmt.Sprintf(`#!/bin/sh
log="${TMPDIR:?}/probe.log"
: > "$log"
note() { printf '%%s %%s\n' "$1" "$2" >> "$log"; }
if ( : > %q ) 2>/dev/null; then note outside-write ok; else note outside-write denied; fi
if ( : > %q ) 2>/dev/null; then note tree-write ok; else note tree-write denied; fi
if command -v bash >/dev/null 2>&1; then
  if bash -c 'exec 3<>/dev/tcp/127.0.0.1/%d' 2>/dev/null; then note connect ok; else note connect denied; fi
  if bash -c 'exec 3<>/dev/udp/127.0.0.1/%d' 2>/dev/null; then note udp ok; else note udp denied; fi
elif command -v python3 >/dev/null 2>&1; then
  if python3 -I -c 'import socket; socket.create_connection(("127.0.0.1", %d), 2).close()' 2>/dev/null; then note connect ok; else note connect denied; fi
  if python3 -I -c 'import socket; s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.connect(("127.0.0.1", %d)); s.close()' 2>/dev/null; then note udp ok; else note udp denied; fi
else
  note connect skipped
  note udp skipped
fi
if %q 2>/dev/null; then note exec ok; else note exec denied; fi
if cp %q "${TMPDIR:?}/copy" && chmod +x "${TMPDIR:?}/copy" && "${TMPDIR:?}/copy" 2>/dev/null; then note copy-exec ok; else note copy-exec denied; fi
if cat %q >/dev/null 2>&1; then note secret ok; else note secret denied; fi
`, c.marker, filepath.Join(c.tree, "canary-wrote"), c.port, c.port, c.port, c.port, filepath.Join(c.tree, "bin", "canary"), filepath.Join(c.tree, "bin", "canary"), c.secret)
}

// Command is the argv to run: sh resolved by the jail, never from the tree.
func (c *Canary) Command() []string { return []string{"sh", c.script} }

// AssertContained checks a confined run that had TMPDIR under scratch: every
// probe ran and was denied (the copy itself landed, only its run did not),
// nothing landed outside scratch, the tree is byte-identical, and the
// listener saw no connection.
func (c *Canary) AssertContained(t testing.TB, scratch string) {
	t.Helper()
	results := readLog(t, filepath.Join(scratch, "tmp", "probe.log"))
	for _, step := range steps {
		if got := results[step]; got != "denied" {
			t.Errorf("probe %s = %q, want denied (a missing or skipped probe proves nothing)", step, got)
		}
	}
	if _, err := os.Stat(c.marker); err == nil {
		t.Errorf("confined process wrote outside scratch: %s", c.marker)
	}
	if _, err := os.Stat(filepath.Join(scratch, "tmp", "exec-ran")); err == nil {
		t.Error("confined process ran bin/canary, from the tree or as a copy in the scratch")
	}
	if _, err := os.Stat(filepath.Join(scratch, "tmp", "copy")); err != nil {
		t.Error("copy-exec never copied bin/canary into the scratch, so its denial proves nothing")
	}
	if got := hashTree(t, c.tree); got != c.treeHash {
		t.Error("confined process changed the tree under review")
	}
	if n := c.accepted.Load(); n != 0 {
		t.Errorf("listener accepted %d connection(s) from the confined process", n)
	}
}

// AssertEscaped runs the same script unconfined and requires the probes to
// succeed, so a pass of AssertContained is the sandbox's doing.
func (c *Canary) AssertEscaped(t testing.TB) {
	t.Helper()
	os.Remove(c.marker)
	tmp := t.TempDir()
	cmd := exec.Command("sh", c.script)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unconfined canary: %v: %s", err, out)
	}
	results := readLog(t, filepath.Join(tmp, "probe.log"))
	for _, step := range steps {
		if got := results[step]; got != "ok" {
			t.Errorf("unconfined probe %s = %q, want ok", step, got)
		}
	}
	if _, err := os.Stat(c.marker); err != nil {
		t.Errorf("unconfined canary left no marker at %s: %v", c.marker, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "exec-ran")); err != nil {
		t.Error("unconfined canary did not run bin/canary")
	}
	if n := c.accepted.Load(); n == 0 {
		t.Error("unconfined canary never reached the listener")
	}
}

func readLog(t testing.TB, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("probe log: %v", err)
	}
	defer f.Close()
	results := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if step, result, ok := strings.Cut(sc.Text(), " "); ok {
			results[step] = result
		}
	}
	return results
}

func hashTree(t testing.TB, tree string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(tree, path)
		fmt.Fprintf(h, "%s\x00", rel)
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mustWrite(t testing.TB, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
