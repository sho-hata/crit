// Package sandbox confines the language servers crit spawns, and the helper
// commands behind them, to the tree under review: no network of any kind,
// writes only under a scratch directory, no execution of files from the
// tree or the scratch directory, no reading of the reviewer's secrets, and
// an allowlisted environment.
//
// macOS uses /usr/bin/sandbox-exec (seatbelt). Elsewhere there is no
// backend yet: a program runs plainly with the scrubbed environment, and the
// caller decides whether the tree it would run on is trusted enough for
// that (crit offers no language server for a PR review there). A backend
// whose self-test failed is not "no backend": nothing runs until it passes.
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
	selfTest      = detect
)

// Detect reports the backend after a self-test. A failed self-test is
// reported as no backend and tried again on the next call; any other answer
// is kept for the process.
func Detect() Backend {
	b, _ := backend()
	return b
}

// backend is Detect plus whether the answer is final.
func backend() (Backend, bool) {
	detectMu.Lock()
	defer detectMu.Unlock()
	if !detectSettled {
		detected, detectSettled = selfTest()
	}
	return detected, detectSettled
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

// Workspace is the root a server runs in and the one directory it may write.
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

// Open refuses a path that is not absolute and clean, and a scratchBase
// under a tree.
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
	// Without a home the secrets cannot be named, and the profile reads
	// everything else: refuse rather than confine nothing.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	return &Jail{trees: t, scratchBase: scratchBase, readDeny: denied(home, scratchBase, t, os.Getenv)}, nil
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

// Command returns a confined command for p, with Dir = Root, and empties the
// scratch's tmp/. Where the OS has no backend the command runs plainly, with
// the scrubbed environment; while a backend's self-test fails it is refused.
func (j *Jail) Command(ctx context.Context, name string, p Process) (*exec.Cmd, error) {
	ws, err := j.mint(name, true)
	if err != nil {
		return nil, err
	}
	return j.command(ctx, p, ws.Scratch, j.trees.Root)
}

// Output runs a short helper (go env, npm root -g) the same way, with Dir =
// scratch, never the tree, and returns its stdout. The scratch is not
// emptied: a server may be using it.
func (j *Jail) Output(ctx context.Context, name string, p Process) ([]byte, error) {
	ws, err := j.mint(name, false)
	if err != nil {
		return nil, err
	}
	cmd, err := j.command(ctx, p, ws.Scratch, ws.Scratch)
	if err != nil {
		return nil, err
	}
	return cmd.Output()
}

func (j *Jail) mint(name string, emptyTmp bool) (Workspace, error) {
	// name is one path element: anything else would put the scratch, and
	// the cleanup below, outside the scratch base.
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return Workspace{}, fmt.Errorf("sandbox: %q is not a scratch name", name)
	}
	ws := j.Workspace(name)
	tmp := filepath.Join(ws.Scratch, "tmp")
	if emptyTmp {
		if err := os.RemoveAll(tmp); err != nil {
			return Workspace{}, fmt.Errorf("sandbox: emptying %s: %w", tmp, err)
		}
	}
	for _, dir := range []string{tmp, filepath.Join(ws.Scratch, "cache")} {
		// The last process could write here: a file or a symlink it left
		// in a directory's place must not fail every later spawn.
		if st, err := os.Lstat(dir); err == nil && !st.IsDir() {
			if err := os.Remove(dir); err != nil {
				return Workspace{}, fmt.Errorf("sandbox: replacing %s: %w", dir, err)
			}
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Workspace{}, fmt.Errorf("sandbox: creating %s: %w", dir, err)
		}
	}
	return ws, nil
}

func (j *Jail) command(ctx context.Context, p Process, scratch, dir string) (*exec.Cmd, error) {
	pol, err := plan(j.trees, j.scratchBase, scratch, dir, p, j.readDeny, os.Environ())
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	switch b, settled := backend(); {
	case b.Name != "":
		if cmd, err = confine(ctx, pol); err != nil {
			return nil, err
		}
	case !settled:
		return nil, fmt.Errorf("sandbox: not running %s unconfined: %s", pol.argv[0], b.Reason)
	default:
		cmd = exec.CommandContext(ctx, pol.argv[0], pol.argv[1:]...) //nolint:gosec // argv[0] is absolute and was refused under the trees by plan
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
		// Beneath cleans before it resolves symlinks, the kernel resolves
		// first: /link/../tool with /link -> <tree>/sub would pass the check
		// as /tool and run <tree>/tool.
		if filepath.Clean(name) != name {
			return "", fmt.Errorf("sandbox: %q must be a clean path", name)
		}
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
// names what the kernel will see (/private/var, not /var). A symlink whose
// target is missing is followed too: the target may appear later (a secret
// on a volume not mounted yet), and the kernel will see that, not the link.
func realPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	// Component by component, as the kernel walks: a ".." in a link's
	// target applies to what the components before it resolve to, so the
	// target is never cleaned first.
	const maxLinks = 32
	sep := string(filepath.Separator)
	vol := filepath.VolumeName(p)
	dir, rest := vol+sep, strings.Split(p[len(vol):], sep)
	for links := 0; len(rest) > 0; {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			dir = filepath.Dir(dir)
			continue
		}
		next := filepath.Join(dir, c)
		target, err := os.Readlink(next)
		if err != nil || links >= maxLinks {
			dir = next
			continue
		}
		links++
		if filepath.IsAbs(target) {
			vol = filepath.VolumeName(target)
			dir, target = vol+sep, target[len(vol):]
		}
		rest = append(strings.Split(filepath.FromSlash(target), sep), rest...)
	}
	return dir
}
