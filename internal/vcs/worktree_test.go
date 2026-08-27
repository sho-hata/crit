package vcs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddSparseWorktreeChecksOutOnlyMatchingFiles(t *testing.T) {
	t.Parallel()
	repo := InitTestRepo(t)
	writeFileForTest(t, filepath.Join(repo, "go.mod"), "module example.com/x\n")
	writeFileForTest(t, filepath.Join(repo, "pkg", "a.go"), "package pkg\n")
	writeFileForTest(t, filepath.Join(repo, "web", "app.js"), "// js\n")
	sha := CommitAtForTest(t, repo, "docs.md", "# docs", "add files")
	// Move the working tree on so the worktree provably reflects sha, not HEAD.
	CommitAtForTest(t, repo, "pkg/a.go", "package pkg // changed\n", "later")

	dir := filepath.Join(t.TempDir(), "wt")
	if err := AddSparseWorktree(context.Background(), repo, sha, dir, []string{"*.go", "go.mod"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"go.mod", "pkg/a.go"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s missing from sparse worktree: %v", want, err)
		}
	}
	for _, absent := range []string{"web/app.js", "docs.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, absent)); err == nil {
			t.Errorf("%s should not be checked out", absent)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pkg/a.go"))
	if string(got) != "package pkg\n" {
		t.Errorf("worktree content = %q, want content at %s", got, sha[:7])
	}

	if err := RemoveWorktree(context.Background(), repo, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("worktree directory still exists after RemoveWorktree")
	}
	if list := GitRun(t, repo, "worktree", "list", "--porcelain"); strings.Contains(list, dir) {
		t.Errorf("worktree metadata not pruned:\n%s", list)
	}
}

func TestAddSparseWorktreeResolvesAbbreviatedSHA(t *testing.T) {
	t.Parallel()
	repo := InitTestRepo(t)
	sha := CommitAtForTest(t, repo, "a.go", "package a\n", "add a.go")
	dir := filepath.Join(t.TempDir(), "wt")
	if err := AddSparseWorktree(context.Background(), repo, sha[:10], dir, []string{"*.go"}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = RemoveWorktree(context.Background(), repo, dir) }()
	if got := GitRun(t, dir, "rev-parse", "HEAD"); got != sha {
		t.Errorf("worktree HEAD = %s, want %s", got, sha)
	}
}

func TestAddSparseWorktreeRejectsHexBranchName(t *testing.T) {
	t.Parallel()
	repo := InitTestRepo(t)
	CommitAtForTest(t, repo, "a.go", "package a\n", "add a.go")
	// A branch whose name passes isCommitish. git resolves "beef" to the
	// branch tip, which is not what a caller passing a SHA meant.
	GitRun(t, repo, "branch", "beef", "HEAD~1")
	if strings.HasPrefix(GitRun(t, repo, "rev-parse", "HEAD~1"), "beef") {
		t.Skip("commit SHA genuinely starts with beef")
	}
	dir := filepath.Join(t.TempDir(), "wt")
	if err := AddSparseWorktree(context.Background(), repo, "beef", dir, []string{"*.go"}); err == nil {
		_ = RemoveWorktree(context.Background(), repo, dir)
		t.Fatal("expected error: hex-looking branch name must not be checked out as a SHA")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("failed add left the worktree directory behind")
	}
}

func TestAddSparseWorktreeRefusesExistingDir(t *testing.T) {
	t.Parallel()
	repo := InitTestRepo(t)
	sha := GitRun(t, repo, "rev-parse", "HEAD")
	dir := t.TempDir()
	keep := filepath.Join(dir, "precious.txt")
	writeFileForTest(t, keep, "do not delete")
	if err := AddSparseWorktree(context.Background(), repo, sha, dir, []string{"*.go"}); err == nil {
		t.Fatal("expected error for existing directory")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("pre-existing file was deleted: %v", err)
	}
}

func TestAddSparseWorktreeRejectsBadInput(t *testing.T) {
	t.Parallel()
	repo := InitTestRepo(t)
	dir := filepath.Join(t.TempDir(), "wt")
	sha := GitRun(t, repo, "rev-parse", "HEAD")
	cases := []struct {
		name     string
		sha      string
		patterns []string
	}{
		{"non-SHA", "not-a-sha", []string{"*.go"}},
		{"unknown commit", "0123456789abcdef0123456789abcdef01234567", []string{"*.go"}},
		{"empty patterns", sha, nil},
		{"anchored pattern", sha, []string{"/go.mod"}},
		{"directory pattern", sha, []string{"cmd/*.go"}},
		{"negated pattern", sha, []string{"*.go", "!vendor"}},
		{"malformed glob", sha, []string{"[go"}},
	}
	for _, tc := range cases {
		if err := AddSparseWorktree(context.Background(), repo, tc.sha, dir, tc.patterns); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%s: failed add left the worktree directory behind", tc.name)
		}
	}
}

func TestAddSparseWorktreeRelativeDir(t *testing.T) {
	repo := InitTestRepo(t)
	sha := CommitAtForTest(t, repo, "a.go", "package a\n", "add a.go")
	// cwd != repoRoot: a relative dir must resolve against cwd for both git
	// and os calls, otherwise RemoveWorktree misses it and leaks.
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := AddSparseWorktree(context.Background(), repo, sha, "rel/wt", []string{"*.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "rel", "wt", "a.go")); err != nil {
		t.Fatalf("worktree not created relative to cwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "rel", "wt")); err == nil {
		t.Error("worktree was created relative to repoRoot")
	}
	if err := RemoveWorktree(context.Background(), repo, "rel/wt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "rel", "wt")); err == nil {
		t.Error("relative worktree leaked after RemoveWorktree")
	}
}

func TestRemoveWorktreeIsIdempotent(t *testing.T) {
	t.Parallel()
	repo := InitTestRepo(t)
	dir := filepath.Join(t.TempDir(), "never-created")
	if err := RemoveWorktree(context.Background(), repo, dir); err != nil {
		t.Fatalf("removing a non-existent worktree: %v", err)
	}
}

func TestRemoveWorktreeRefusesForeignDir(t *testing.T) {
	t.Parallel()
	repo := InitTestRepo(t)
	dir := t.TempDir()
	keep := filepath.Join(dir, "precious.txt")
	writeFileForTest(t, keep, "do not delete")
	if err := RemoveWorktree(context.Background(), repo, dir); err == nil {
		t.Error("expected error removing a directory that is not a worktree")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("foreign directory was deleted: %v", err)
	}
}

func TestRemoveWorktreeDoesNotPruneOtherWorktrees(t *testing.T) {
	t.Parallel()
	repo := InitTestRepo(t)
	sha := CommitAtForTest(t, repo, "a.go", "package a\n", "add a.go")
	// Simulate the user's own worktree on a temporarily unavailable volume:
	// registered with git, directory missing.
	other := filepath.Join(t.TempDir(), "other")
	GitRun(t, repo, "worktree", "add", "--detach", other, sha)
	if err := os.RemoveAll(other); err != nil {
		t.Fatal(err)
	}

	ours := filepath.Join(t.TempDir(), "wt")
	if err := AddSparseWorktree(context.Background(), repo, sha, ours, []string{"*.go"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveWorktree(context.Background(), repo, ours); err != nil {
		t.Fatal(err)
	}
	if list := GitRun(t, repo, "worktree", "list", "--porcelain"); !strings.Contains(list, "other") {
		t.Errorf("user's missing worktree was pruned:\n%s", list)
	}
}

func TestSparseTreeSizeMatchesCheckout(t *testing.T) {
	t.Parallel()
	repo := InitTestRepo(t)
	writeFileForTest(t, filepath.Join(repo, "go.mod"), "module example.com/x\n")
	writeFileForTest(t, filepath.Join(repo, "pkg", "a.go"), "package pkg\n\nfunc A() {}\n")
	writeFileForTest(t, filepath.Join(repo, "pkg", "日本語.go"), "package pkg // non-ASCII path\n")
	writeFileForTest(t, filepath.Join(repo, "web", "big.bin"), string(make([]byte, 4096)))
	sha := CommitAtForTest(t, repo, "pkg/b.go", "package pkg\n", "add files")

	patterns := []string{"*.go", "go.mod"}
	est, err := SparseTreeSize(context.Background(), repo, sha, patterns)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(len("module example.com/x\n") + len("package pkg\n\nfunc A() {}\n") +
		len("package pkg // non-ASCII path\n") + len("package pkg\n"))
	if est != want {
		t.Errorf("SparseTreeSize = %d, want %d", est, want)
	}

	dir := filepath.Join(t.TempDir(), "wt")
	if err := AddSparseWorktree(context.Background(), repo, sha, dir, patterns); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = RemoveWorktree(context.Background(), repo, dir) }()
	var actual int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}
		// In a linked worktree .git is a file pointing at the main repo.
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				actual += info.Size()
			}
		}
		return nil
	})
	if actual != est {
		t.Errorf("checked-out bytes %d != estimate %d", actual, est)
	}
}
