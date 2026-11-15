package sandbox

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sho-hata/crit/internal/sandbox/sandboxtest"
)

func TestBuildEnv(t *testing.T) {
	t.Parallel()
	// Real absolute paths: a bare "/usr/bin" is not absolute on Windows.
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	scratch := filepath.Join(base, "scratch")
	trees := Trees{Root: root}
	usrBin := filepath.Join(base, "usr", "bin")
	environ := []string{
		"PATH=" + strings.Join([]string{usrBin, filepath.Join(root, "node_modules", ".bin"), "relative/bin"}, string(os.PathListSeparator)),
		"HOME=/home/u", "LANG=C.UTF-8", "GOFLAGS=-mod=mod",
		"GITHUB_TOKEN=ghp_x", "AWS_SECRET_ACCESS_KEY=k", "NPM_TOKEN=t", "EDITOR=vim", "TMPDIR=/tmp/outside",
		"MY_SDK=/opt/sdk", "GOPROXY=https://user:token@proxy.example",
	}

	env, err := buildEnv(environ, trees, scratch, Process{Env: []string{"GOCACHE=" + filepath.Join(scratch, "gocache")}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["PATH"] != usrBin {
		t.Errorf("PATH = %q, want only %q (in-tree and relative entries dropped)", got["PATH"], usrBin)
	}
	for _, k := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "NPM_TOKEN", "EDITOR", "MY_SDK", "GOPROXY"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s leaked through the allowlist", k)
		}
	}
	if got["HOME"] != "/home/u" || got["LANG"] != "C.UTF-8" || got["GOFLAGS"] != "-mod=mod" {
		t.Errorf("allowlisted values not copied: %v", got)
	}
	tmp := filepath.Join(scratch, "tmp")
	if got["TMPDIR"] != tmp || got["TMP"] != tmp || got["TEMP"] != tmp {
		t.Errorf("temp dirs = %q %q %q, want %q", got["TMPDIR"], got["TMP"], got["TEMP"], tmp)
	}
	if got["XDG_CACHE_HOME"] != filepath.Join(scratch, "cache") || got["GOTOOLCHAIN"] != "local" {
		t.Errorf("cache/toolchain = %q %q", got["XDG_CACHE_HOME"], got["GOTOOLCHAIN"])
	}
	if got["GOCACHE"] != filepath.Join(scratch, "gocache") {
		t.Errorf("Process.Env not appended: GOCACHE = %q", got["GOCACHE"])
	}

	for _, kv := range []string{"PATH=/evil", "HOME=/x", "TMPDIR=/x", "tmp=/x"} {
		if _, err := buildEnv(environ, trees, scratch, Process{Env: []string{kv}}); err == nil {
			t.Errorf("Env %q must be refused", kv)
		}
	}
}

func TestSecretsList(t *testing.T) {
	t.Parallel()
	home := filepath.Join(string(filepath.Separator), "home", "u")
	got := secrets(home, "linux")
	for _, rel := range []string{
		".ssh", ".aws", ".gnupg", ".netrc", ".npmrc", ".kube", ".docker", ".config/gh",
		".git-credentials", ".config/git/credentials",
		".terraform.d/credentials.tfrc.json", ".terraformrc",
		".composer/auth.json", ".config/composer/auth.json",
		".config/pip", ".pip",
		".bundle/config", ".pgpass", ".vault-token", ".password-store",
		".local/share/keyrings", ".config/op", ".config/hub",
		".claude", ".claude.json", ".codex", ".zsh_history", ".bash_history",
		".crit", ".crit.config.json",
	} {
		if !slices.Contains(got, filepath.Join(home, filepath.FromSlash(rel))) {
			t.Errorf("secrets lacks %s", rel)
		}
	}
	if slices.Contains(got, filepath.Join(home, "Library", "Keychains")) {
		t.Error("Keychains is darwin-only")
	}
	for _, rel := range []string{"Library/Keychains", "Library/Application Support/pip"} {
		if !slices.Contains(secrets(home, "darwin"), filepath.Join(home, filepath.FromSlash(rel))) {
			t.Errorf("darwin must deny %s", rel)
		}
	}
	if secrets("", "linux") != nil {
		t.Error("no home, no home-relative secrets")
	}
}

// The reviews directory (two levels above the scratch base) joins the deny
// set, except where denying it would cover the root, the home, or a tree.
func TestDeniedAddsTheReviewsDir(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	home := filepath.Join(base, "home", "u")
	repo := filepath.Join(base, "src", "repo")
	for _, dir := range []string{home, repo} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	reviews := filepath.Join(base, "data", "reviews")
	scratch := filepath.Join(reviews, "k", "lsp-scratch")

	if got := denied(home, scratch, Trees{Root: repo}); !slices.Contains(got, reviews) {
		t.Errorf("reviews dir %s not denied: %v", reviews, got)
	}
	if got := denied(home, scratch, Trees{Root: repo}); !slices.Contains(got, filepath.Join(home, ".ssh")) {
		t.Error("the secrets must stay in the set")
	}
	// Range focus: the root is a worktree inside the reviews dir, the working
	// tree is elsewhere; other reviews must stay unreadable.
	if got := denied(home, scratch, Trees{Root: filepath.Join(reviews, "k", "lsp-worktree"), DepRoot: repo}); !slices.Contains(got, reviews) {
		t.Errorf("reviews dir %s not denied under range focus: %v", reviews, got)
	}
	cases := []struct {
		name    string
		scratch string
		trees   Trees
	}{
		{"root", filepath.Join(string(filepath.Separator), "k", "lsp-scratch"), Trees{Root: repo}},
		{"home", filepath.Join(home, "k", "lsp-scratch"), Trees{Root: repo}},
		{"ancestor of home", filepath.Join(base, "home", "k", "lsp-scratch"), Trees{Root: repo}},
		{"ancestor of the working tree", filepath.Join(base, "src", "k", "lsp-scratch"), Trees{Root: repo}},
		{"ancestor of the dep root", scratch, Trees{Root: repo, DepRoot: filepath.Join(reviews, "other")}},
	}
	for _, tc := range cases {
		got := denied(home, tc.scratch, tc.trees)
		reviews := filepath.Dir(filepath.Dir(tc.scratch))
		if slices.Contains(got, reviews) {
			t.Errorf("%s: %s must not be denied", tc.name, reviews)
		}
	}
}

type fakeEntry struct {
	name string
	dir  bool
}

func (e fakeEntry) Name() string               { return e.name }
func (e fakeEntry) IsDir() bool                { return e.dir }
func (e fakeEntry) Type() fs.FileMode          { return 0 }
func (e fakeEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrInvalid }

func TestProfileGolden(t *testing.T) {
	t.Parallel()
	pol := policy{
		readDeny:    []string{"/Users/u/.ssh", "/Users/u/.crit"},
		trees:       []string{"/Users/u/.crit/reviews/k/lsp-worktree", "/Users/u/repo"},
		scratchBase: "/Users/u/.crit/reviews/k/lsp-scratch",
		scratch:     "/Users/u/.crit/reviews/k/lsp-scratch/go/abc",
	}
	text, params := profile(pol)
	wantParams := []string{
		"ROOT=/Users/u/.crit/reviews/k/lsp-worktree", "DEP_ROOT=/Users/u/repo",
		"SCRATCH_BASE=/Users/u/.crit/reviews/k/lsp-scratch", "SCRATCH=/Users/u/.crit/reviews/k/lsp-scratch/go/abc",
		"READ_DENY_0=/Users/u/.ssh", "READ_DENY_1=/Users/u/.crit",
		"READ_ALLOW_0=/Users/u/repo", "READ_ALLOW_1=/Users/u/.crit/reviews/k/lsp-worktree",
		"READ_ALLOW_2=/Users/u/.crit/reviews/k/lsp-scratch/go/abc",
	}
	if !slices.Equal(params, wantParams) {
		t.Errorf("params = %q\nwant %q", params, wantParams)
	}
	wantReads := `(deny file-read* (subpath (param "READ_DENY_0")) (subpath (param "READ_DENY_1")))
(allow file-read* (subpath (param "READ_ALLOW_0")))
(allow file-read* (subpath (param "READ_ALLOW_1")))
(allow file-read* (subpath (param "READ_ALLOW_2")))
(allow file-read-metadata (vnode-type DIRECTORY))
`
	if text != baseProfile+wantReads {
		t.Errorf("profile = profile.sb +\n%s\nwant profile.sb +\n%s", strings.TrimPrefix(text, baseProfile), wantReads)
	}
	for _, denied := range []string{"network", "mach-lookup (global-name \"com.apple.coreservices", "default"} {
		if strings.Contains(text, "(allow "+denied) {
			t.Errorf("profile allows %s", denied)
		}
	}
	for _, name := range []string{"SecurityServer", "cfprefsd", "diagnosticd", "analyticsd", "trustd", "/dev/tty"} {
		if strings.Contains(text, name) {
			t.Errorf("profile still names %s", name)
		}
	}
	if strings.Contains(text, "/Users/u") {
		t.Error("paths must travel as -D params, not in the profile text")
	}
}

// profile.sb and profile.go agree on the parameters: each one the text reads
// is passed, and each one passed is read. sandbox-exec refuses an unset one
// at spawn, which only the macOS job would see.
func TestProfileParamsMatch(t *testing.T) {
	t.Parallel()
	text, params := profile(policy{
		readDeny:    []string{"/h/.ssh"},
		trees:       []string{"/r"},
		scratchBase: "/s",
		scratch:     "/s/x",
	})
	var used []string
	for _, m := range regexp.MustCompile(`\(param "([A-Z_0-9]+)"\)`).FindAllStringSubmatch(text, -1) {
		used = append(used, m[1])
	}
	var passed []string
	for _, kv := range params {
		k, _, _ := strings.Cut(kv, "=")
		passed = append(passed, k)
	}
	slices.Sort(used)
	used = slices.Compact(used)
	slices.Sort(passed)
	if !slices.Equal(used, passed) {
		t.Errorf("profile reads %q, profile() passes %q", used, passed)
	}
}

// A secret beneath a tree (a dotfiles repository checked out at $HOME) is
// denied again right after the tree is allowed, and a deeper allow (the
// scratch under ~/.crit) still comes after that.
func TestProfileRedeniesSecretsUnderATree(t *testing.T) {
	t.Parallel()
	pol := policy{
		readDeny:    []string{"/Users/u/.ssh", "/Users/u/.crit", "/Users/u/.crit/reviews"},
		trees:       []string{"/Users/u/.crit/reviews/k/lsp-worktree", "/Users/u"},
		scratchBase: "/Users/u/.crit/reviews/k/lsp-scratch",
		scratch:     "/Users/u/.crit/reviews/k/lsp-scratch/go/abc",
	}
	text, _ := profile(pol)
	want := `(deny file-read* (subpath (param "READ_DENY_0")) (subpath (param "READ_DENY_1")) (subpath (param "READ_DENY_2")))
(allow file-read* (subpath (param "READ_ALLOW_0")))
(deny file-read* (subpath (param "READ_DENY_0")) (subpath (param "READ_DENY_1")) (subpath (param "READ_DENY_2")))
(allow file-read* (subpath (param "READ_ALLOW_1")))
(allow file-read* (subpath (param "READ_ALLOW_2")))
(allow file-read-metadata (vnode-type DIRECTORY))
`
	if !strings.HasSuffix(text, want) {
		t.Errorf("profile =\n%s\nwant it to end with\n%s", text, want)
	}
}

// requireBackend skips without a sandbox backend, or fails when CI demands one.
func requireBackend(t *testing.T) Backend {
	t.Helper()
	b := Detect()
	if b.Name == "" {
		if os.Getenv("CRIT_REQUIRE_SANDBOX") != "" {
			t.Fatalf("CRIT_REQUIRE_SANDBOX is set and there is no backend: %s", b.Reason)
		}
		t.Skipf("no sandbox backend: %s", b.Reason)
	}
	return b
}

func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenRejectsBadPaths(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	if _, err := Open(Trees{Root: "relative"}, realTempDir(t)); err == nil {
		t.Error("relative root accepted")
	}
	if _, err := Open(Trees{Root: root}, filepath.Join(root, "scratch")); err == nil {
		t.Error("scratch under the tree accepted")
	}
	if _, err := Open(Trees{Root: root + string(filepath.Separator)}, realTempDir(t)); err == nil {
		t.Error("unclean root accepted")
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(realTempDir(t), "link")
		if err := os.Symlink(root, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(Trees{Root: root}, filepath.Join(link, "scratch")); err == nil {
			t.Error("scratch reached through a symlink into the tree accepted")
		}
	}
}

// No home, no secrets to name: refused rather than left readable.
func TestOpenRefusesWithoutAHome(t *testing.T) {
	homeVar := "HOME"
	if runtime.GOOS == "windows" {
		homeVar = "USERPROFILE"
	}
	t.Setenv(homeVar, "")
	if _, err := Open(Trees{Root: realTempDir(t)}, realTempDir(t)); err == nil {
		t.Error("Open without a home directory accepted")
	}
}

func TestCommandRefusesProgramFromTheTree(t *testing.T) {
	root := realTempDir(t)
	evil := filepath.Join(root, "evil")
	if err := os.WriteFile(evil, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := Open(Trees{Root: root}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	proc := func(argv ...string) func(Workspace) Process {
		return func(Workspace) Process { return Process{Argv: argv} }
	}
	if _, err := j.Command(context.Background(), "t", proc(evil)); err == nil {
		t.Error("absolute path into the tree accepted")
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := j.Command(context.Background(), "t", proc("evil")); err == nil {
		t.Error("bare name found through an in-tree PATH entry accepted")
	}
	if _, err := j.Command(context.Background(), "t", proc("sub/evil")); err == nil {
		t.Error("relative path accepted")
	}
	if runtime.GOOS != "windows" {
		// /outside/link -> <tree>/sub, so /outside/link/../evil is <tree>/evil
		// to the kernel and /outside/evil once cleaned.
		if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(realTempDir(t), "link")
		if err := os.Symlink(filepath.Join(root, "sub"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := j.Command(context.Background(), "t", proc(link+"/../evil")); err == nil {
			t.Error("unclean absolute path into the tree accepted")
		}
	}
}

// A scratch that is no longer a directory (a confined process swapped it for
// a symlink) is refused before tmp/ is emptied through the link.
func TestCommandRefusesReplacedScratch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	t.Parallel()
	j, err := Open(Trees{Root: realTempDir(t)}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ws := j.Workspace("t")
	victim := realTempDir(t)
	keep := filepath.Join(victim, "tmp", "keep")
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(ws.Scratch), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, ws.Scratch); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Command(context.Background(), "t", func(Workspace) Process {
		return Process{Argv: []string{"sh"}}
	}); err == nil {
		t.Error("symlinked scratch accepted")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("cleanup followed the link into %s: %v", victim, err)
	}
}

// The process sees the scrubbed environment and the scratch temp dir
// whether or not a backend exists; Command and Output differ in cwd.
func TestCommandEnvironmentAndCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	t.Setenv("CANARY_TOKEN", "leak")
	t.Setenv("LANG", "C")
	root := realTempDir(t)
	j, err := Open(Trees{Root: root}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ws := j.Workspace("t")
	probe := func(Workspace) Process {
		return Process{Argv: []string{"sh", "-c", `printf '%s\n%s\n%s\n%s\n' "$PWD" "$TMPDIR" "${CANARY_TOKEN-unset}" "$LANG"`}}
	}
	cmd, err := j.Command(context.Background(), "t", probe)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v (backend %+v)", err, Detect())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := []string{root, filepath.Join(ws.Scratch, "tmp"), "unset", "C"}
	if !slices.Equal(lines, want) {
		t.Errorf("Command sees %q, want %q", lines, want)
	}
	if st, err := os.Stat(ws.Scratch); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("scratch %s: %v, mode %v, want 0700", ws.Scratch, err, st.Mode())
	}

	out, err = j.Output(context.Background(), "t", probe)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); lines[0] != ws.Scratch {
		t.Errorf("Output cwd = %q, want the scratch %q", lines[0], ws.Scratch)
	}
}

func TestCommandEmptiesTmpOnEachSpawn(t *testing.T) {
	t.Parallel()
	j, err := Open(Trees{Root: realTempDir(t)}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ws := j.Workspace("t")
	stale := filepath.Join(ws.Scratch, "tmp", "stale")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Command(context.Background(), "t", func(Workspace) Process {
		return Process{Argv: []string{"/nonexistent/bin"}}
	}); err == nil {
		t.Fatal("nonexistent program accepted")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("tmp/ not emptied: %v", err)
	}
}

// A file one server wrote into its own scratch does not run from another
// server's jail either: the scratch base as a whole is exec-denied.
func TestCommandRefusesExecFromAnotherScratch(t *testing.T) {
	requireBackend(t)
	j, err := Open(Trees{Root: realTempDir(t)}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(j.Workspace("other").Scratch, "true")
	data, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o700); err != nil { //nolint:gosec // an executable copy is the point
		t.Fatal(err)
	}
	if err := exec.Command(bin).Run(); err != nil { //nolint:gosec // control: the copy runs unconfined
		t.Fatalf("control: the copy does not run unconfined: %v", err)
	}
	cmd, err := j.Command(context.Background(), "t", func(Workspace) Process {
		return Process{Argv: []string{"sh", "-c", `"$0"`, bin}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("%s ran from another scratch under the jail (%q)", bin, out)
	}
}

// A confined process may write under its scratch but not replace the scratch
// itself: a symlink there would aim the next spawn's cleanup and write grant
// elsewhere.
func TestJailKeepsTheScratchInPlace(t *testing.T) {
	requireBackend(t)
	j, err := Open(Trees{Root: realTempDir(t)}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ws := j.Workspace("t")
	victim := realTempDir(t)
	cmd, err := j.Command(context.Background(), "t", func(Workspace) Process {
		return Process{Argv: []string{"sh", "-c", `rm -rf "$0"; mv "$0" "$0.moved"; ln -s "$1" "$0"; : > "$0/after"`, ws.Scratch, victim}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if st, err := os.Lstat(ws.Scratch); err != nil || !st.IsDir() {
		t.Fatalf("scratch %s replaced: %v %v", ws.Scratch, err, st)
	}
	if _, err := os.Lstat(ws.Scratch + ".moved"); err == nil {
		t.Error("scratch renamed away")
	}
	// Writes under the scratch still land, in the scratch and not the victim.
	if _, err := os.Stat(filepath.Join(ws.Scratch, "after")); err != nil {
		t.Errorf("write under the scratch denied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(victim, "after")); err == nil {
		t.Error("write landed in the symlink target")
	}
	if _, err := j.Command(context.Background(), "t", func(Workspace) Process { return Process{Argv: []string{"sh"}} }); err != nil {
		t.Errorf("respawn after the attempt: %v", err)
	}
}

// TestJailConfinesCanary is the contract: the same script escapes when run
// plainly (the control) and is contained through the Jail.
func TestJailConfinesCanary(t *testing.T) {
	b := requireBackend(t)
	tree := realTempDir(t)
	c := sandboxtest.Plant(t, tree)
	j, err := Open(Trees{Root: tree}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ws := j.Workspace("canary")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd, err := j.Command(ctx, "canary", func(Workspace) Process {
		return Process{Argv: c.Command()}
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	t.Logf("backend %+v; confined canary: err=%v out=%q", b, err, out)
	c.AssertContained(t, ws.Scratch)
	c.AssertEscaped(t)
}
