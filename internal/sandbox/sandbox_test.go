package sandbox

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
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
		"MY_SDK=/opt/sdk",
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
	for _, k := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "NPM_TOKEN", "EDITOR", "MY_SDK"} {
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
		".bundle/config", ".pgpass", ".vault-token", ".password-store",
		".local/share/keyrings", ".config/op", ".config/hub",
		".claude", ".claude.json", ".zsh_history", ".bash_history",
		".crit", ".crit.config.json",
	} {
		if !slices.Contains(got, filepath.Join(home, filepath.FromSlash(rel))) {
			t.Errorf("secrets lacks %s", rel)
		}
	}
	if slices.Contains(got, filepath.Join(home, "Library", "Keychains")) {
		t.Error("Keychains is darwin-only")
	}
	if !slices.Contains(secrets(home, "darwin"), filepath.Join(home, "Library", "Keychains")) {
		t.Error("darwin must deny Library/Keychains")
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
		readDeny:  []string{"/Users/u/.ssh", "/Users/u/.crit"},
		readAllow: []string{"/Users/u/.crit/reviews/k/lsp-worktree", "/Users/u/.crit/reviews/k/lsp-scratch/go/abc"},
		execDeny:  []string{"/Users/u/.crit/reviews/k/lsp-worktree", "/Users/u/repo"},
		scratch:   "/Users/u/.crit/reviews/k/lsp-scratch/go/abc",
	}
	text, params := profile(pol)
	wantParams := []string{
		"SCRATCH_0=/Users/u/.crit/reviews/k/lsp-scratch/go/abc",
		"EXEC_DENY_0=/Users/u/.crit/reviews/k/lsp-worktree", "EXEC_DENY_1=/Users/u/repo",
		"READ_DENY_0=/Users/u/.ssh", "READ_DENY_1=/Users/u/.crit",
		"READ_ALLOW_0=/Users/u/.crit/reviews/k/lsp-worktree", "READ_ALLOW_1=/Users/u/.crit/reviews/k/lsp-scratch/go/abc",
	}
	if !slices.Equal(params, wantParams) {
		t.Errorf("params = %q\nwant %q", params, wantParams)
	}
	wantHead := `(version 1)
(deny default)
(allow process-fork)
(allow process-exec)
(deny process-exec (subpath (param "EXEC_DENY_0")) (subpath (param "EXEC_DENY_1")) (subpath (param "SCRATCH_0")))
(allow file-map-executable)
(deny file-map-executable (subpath (param "EXEC_DENY_0")) (subpath (param "EXEC_DENY_1")) (subpath (param "SCRATCH_0")))
(allow signal (target same-sandbox))
(allow file-read*)
(deny file-read* (subpath (param "READ_DENY_0")) (subpath (param "READ_DENY_1")))
(allow file-read* (subpath (param "READ_ALLOW_0")))
(allow file-read* (subpath (param "READ_ALLOW_1")))
(allow file-read-metadata (vnode-type DIRECTORY))
(allow file-write* (subpath (param "SCRATCH_0")))
(allow file-write-data (literal "/dev/null") (literal "/dev/zero"))
(allow file-ioctl (literal "/dev/null") (literal "/dev/zero") (literal "/dev/random") (literal "/dev/urandom") (literal "/dev/dtracehelper"))
`
	if !strings.HasPrefix(text, wantHead) {
		t.Errorf("profile head =\n%s\nwant\n%s", text, wantHead)
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

// A secret beneath a tree (a dotfiles repository checked out at $HOME) is
// denied again right after the tree is allowed, and a deeper allow (the
// scratch under ~/.crit) still comes after that.
func TestProfileRedeniesSecretsUnderATree(t *testing.T) {
	t.Parallel()
	pol := policy{
		readDeny:  []string{"/Users/u/.ssh", "/Users/u/.crit", "/Users/u/.crit/reviews"},
		readAllow: []string{"/Users/u/.crit/reviews/k/lsp-worktree", "/Users/u", "/Users/u/.crit/reviews/k/lsp-scratch/go/abc"},
		execDeny:  []string{"/Users/u/.crit/reviews/k/lsp-worktree", "/Users/u"},
		scratch:   "/Users/u/.crit/reviews/k/lsp-scratch/go/abc",
	}
	text, _ := profile(pol)
	want := `(deny file-read* (subpath (param "READ_DENY_0")) (subpath (param "READ_DENY_1")) (subpath (param "READ_DENY_2")))
(allow file-read* (subpath (param "READ_ALLOW_0")))
(deny file-read* (subpath (param "READ_DENY_0")) (subpath (param "READ_DENY_1")) (subpath (param "READ_DENY_2")))
(allow file-read* (subpath (param "READ_ALLOW_1")))
(allow file-read* (subpath (param "READ_ALLOW_2")))
(allow file-read-metadata (vnode-type DIRECTORY))
`
	if !strings.Contains(text, want) {
		t.Errorf("profile =\n%s\nwant it to contain\n%s", text, want)
	}
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

// TestJailConfinesCanary is the contract: the same script escapes when run
// plainly (the control) and is contained through the Jail.
func TestJailConfinesCanary(t *testing.T) {
	b := Detect()
	if b.Name == "" {
		if os.Getenv("CRIT_REQUIRE_SANDBOX") != "" {
			t.Fatalf("CRIT_REQUIRE_SANDBOX is set and there is no backend: %s", b.Reason)
		}
		t.Skipf("no sandbox backend: %s", b.Reason)
	}
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
