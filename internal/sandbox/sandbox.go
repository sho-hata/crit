// Package sandbox confines the language servers crit spawns, and the helper
// commands behind them, to the tree under review: no network of any kind,
// writes only under a scratch directory, no execution of files from the
// tree or the scratch directory, no reading of the reviewer's secrets, and
// an allowlisted environment.
//
// macOS uses /usr/bin/sandbox-exec (seatbelt). Elsewhere there is no
// backend yet: a program runs plainly with the scrubbed environment, and the
// caller decides whether the tree it would run on is trusted enough for
// that (crit offers no language server for a PR review there).
//
// Containment, not prevention: a confined process cannot execute or, on
// macOS, load as native code any file from the tree or the scratch, but
// `sh <tree>/x.sh` still runs through the interpreter, inside the jail.
// No config key, flag or environment variable turns the confinement off.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Backend is the platform's confinement. Name is "seatbelt" or "" for none;
// Reason explains "".
type Backend struct{ Name, Reason string }

var (
	detectMu      sync.Mutex
	detected      Backend
	detectSettled bool
)

// Detect reports the backend after a real self-test: a confined no-op must
// run. A definitive answer (no sandbox on this OS, no sandbox-exec) is kept
// for the process.
// A failed self-test is reported as no backend and tried again on the next
// call, so one failed fork at startup does not leave the servers unconfined
// for the daemon's life.
func Detect() Backend {
	detectMu.Lock()
	defer detectMu.Unlock()
	if !detectSettled {
		detected, detectSettled = detect()
	}
	return detected
}

// Trees is the code under review: readable, never writable, never executable.
// DepRoot is the working tree behind a range-focus worktree, "" when none.
type Trees struct{ Root, DepRoot string }

func (t Trees) list() []string {
	if t.DepRoot == "" || t.DepRoot == t.Root {
		return []string{t.Root}
	}
	return []string{t.Root, t.DepRoot}
}

// Workspace is what a language's hooks see: the root the server runs in and
// the one directory it may write. Only a Jail mints a Scratch.
type Workspace struct{ Root, Scratch string }

// Process is one program to run. Argv[0] is a bare name resolved on the
// scrubbed PATH, or absolute; either way it must not live under a tree.
type Process struct {
	Argv []string
	// Env is appended after the allowlist (KEY=VAL). It may not set PATH,
	// HOME, TMPDIR, TMP or TEMP.
	Env    []string
	Stderr io.Writer
}

// Jail binds the trees and the scratch base; one per Manager, so per root.
type Jail struct {
	trees       Trees
	scratchBase string
	readDeny    []string
}

// Open validates the paths (absolute and clean, scratchBase outside the
// trees) and fixes the read-deny set.
func Open(t Trees, scratchBase string) (*Jail, error) {
	for _, p := range append(t.list(), scratchBase) {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, fmt.Errorf("sandbox: %q must be an absolute, clean path", p)
		}
	}
	for _, tree := range t.list() {
		if Beneath(scratchBase, tree) {
			return nil, fmt.Errorf("sandbox: scratch %s lies under the tree %s", scratchBase, tree)
		}
	}
	// A process without a home has no home-relative secrets to deny.
	home, _ := os.UserHomeDir()
	return &Jail{trees: t, scratchBase: scratchBase, readDeny: denied(home, scratchBase, t)}, nil
}

// Workspace names the scratch directory Command mints for name, so a
// handshake hook can point the server at it before the spawn.
func (j *Jail) Workspace(name string) Workspace {
	sum := sha256.Sum256([]byte(j.trees.Root))
	return Workspace{
		Root:    j.trees.Root,
		Scratch: filepath.Join(j.scratchBase, name, hex.EncodeToString(sum[:])[:12]),
	}
}

// Command returns a confined command for the Process build describes, with
// Dir = Root. It mints the scratch directory (0700, tmp/ emptied). It refuses
// when Argv[0] resolves under a tree or when the policy cannot be built.
// Without a backend the command runs plainly, with the scrubbed environment.
func (j *Jail) Command(ctx context.Context, name string, build func(Workspace) Process) (*exec.Cmd, error) {
	ws, err := j.mint(name, true)
	if err != nil {
		return nil, err
	}
	return j.command(ctx, build(ws), ws.Scratch, j.trees.Root)
}

// Output runs a short helper (go env, npm root -g) the same way, with Dir =
// scratch, never the tree, and returns its stdout. The scratch is not
// emptied: a server may be using it.
func (j *Jail) Output(ctx context.Context, name string, build func(Workspace) Process) ([]byte, error) {
	ws, err := j.mint(name, false)
	if err != nil {
		return nil, err
	}
	cmd, err := j.command(ctx, build(ws), ws.Scratch, ws.Scratch)
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}

func (j *Jail) mint(name string, emptyTmp bool) (Workspace, error) {
	ws := j.Workspace(name)
	tmp := filepath.Join(ws.Scratch, "tmp")
	if emptyTmp {
		if err := os.RemoveAll(tmp); err != nil {
			return Workspace{}, fmt.Errorf("sandbox: emptying %s: %w", tmp, err)
		}
	}
	for _, dir := range []string{tmp, filepath.Join(ws.Scratch, "cache")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Workspace{}, fmt.Errorf("sandbox: creating %s: %w", dir, err)
		}
	}
	return ws, nil
}

func (j *Jail) command(ctx context.Context, p Process, scratch, dir string) (*exec.Cmd, error) {
	pol, err := plan(j.trees, scratch, dir, p, j.readDeny, os.Environ())
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	if Detect().Name == "" {
		cmd = exec.CommandContext(ctx, pol.argv[0], pol.argv[1:]...) //nolint:gosec // argv[0] is absolute and was refused under the trees by plan
	} else if cmd, err = confine(ctx, pol); err != nil {
		return nil, err
	}
	cmd.Dir = pol.dir
	cmd.Env = pol.env
	cmd.Stderr = p.Stderr
	return cmd, nil
}

// resolveArgv0 finds the program on dirs (the scrubbed PATH), or checks an
// absolute path, and refuses anything under a tree.
func resolveArgv0(argv []string, dirs []string, trees []string) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("sandbox: empty argv")
	}
	name := argv[0]
	var bin string
	switch {
	case filepath.IsAbs(name):
		p, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("sandbox: %w", err)
		}
		bin = p
	case strings.ContainsAny(name, `/\`):
		return "", fmt.Errorf("sandbox: %q must be a bare name or an absolute path", name)
	default:
		for _, dir := range dirs {
			if p, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
				bin = p
				break
			}
		}
		if bin == "" {
			return "", fmt.Errorf("sandbox: %s not found on PATH", name)
		}
	}
	for _, tree := range trees {
		if Beneath(bin, tree) {
			return "", fmt.Errorf("sandbox: refusing to run %s from inside the tree under review", bin)
		}
	}
	return bin, nil
}

// Beneath reports whether path is root or lies under it, as given or once
// symlinks are resolved (macOS's /var is /private/var). Both are absolute.
func Beneath(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	return beneath(path, root) || beneath(realPath(path), realPath(root))
}

// beneath is Beneath on paths taken as written. Both are clean.
func beneath(path, root string) bool {
	if path == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(path, prefix)
}

// realPath resolves symlinks through the deepest ancestor that exists, so
// a path under a symlinked directory, or a secret not created yet, still
// names what the kernel will see (/private/var, not /var).
func realPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(realPath(parent), filepath.Base(p))
}
