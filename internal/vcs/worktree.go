package vcs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AddSparseWorktree checks out sha into dir as a detached git worktree that
// contains only the paths matching patterns (git sparse-checkout, non-cone
// mode, so "*.go" matches at every depth). The worktree shares the
// repository's object store, so the on-disk cost is the matched files only.
//
// dir must not exist yet: this function only ever deletes directories it
// created itself. sha is resolved with `rev-parse --verify <sha>^{commit}`
// so a hex-looking branch name or ambiguous prefix cannot check out the
// wrong tree. On any failure the partially created worktree is removed so a
// retry starts clean.
//
// Used to give a language server a real filesystem view of a commit that is
// not the working tree (range/PR focus). Files land byte-for-byte as they are
// in the commit — see noEOLConversion.
func AddSparseWorktree(ctx context.Context, repoRoot, sha, dir string, patterns []string) error {
	dir, err := absWorktreeDir(dir)
	if err != nil {
		return err
	}
	if err := validateSparsePatterns(patterns); err != nil {
		return err
	}
	fullSHA, err := resolveCommit(ctx, repoRoot, sha)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("worktree: %s already exists", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("worktree: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	steps := [][]string{
		{"worktree", "add", "--detach", "--no-checkout", dir, fullSHA},
		append([]string{"-C", dir, "sparse-checkout", "set", "--no-cone"}, patterns...),
		{"-C", dir, "checkout", "--quiet"},
	}
	for _, args := range steps {
		if _, err := runGit(ctx, repoRoot, append(noEOLConversion(), args...)...); err != nil {
			// Own timeout, not ctx: ctx may itself be why this step failed.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), gitOpTimeout)
			_ = removeWorktree(cleanupCtx, repoRoot, dir, true)
			cancel()
			return fmt.Errorf("worktree: %w", err)
		}
	}
	return nil
}

// noEOLConversion returns the git options that keep a checkout byte-identical
// to the blobs it comes from. Without them, a machine with core.autocrlf
// enabled (the default on Windows) rewrites line endings on the way out, so
// the worktree would no longer match the commit content the review pane
// renders — and SparseTreeSize, which sums blob sizes, would under-count what
// actually lands on disk. An explicit .gitattributes eol still wins, as it
// should: that is the repository declaring its own on-disk form.
func noEOLConversion() []string {
	return []string{"-c", "core.autocrlf=false", "-c", "core.eol=lf"}
}

// RemoveWorktree is idempotent (a missing worktree is not an error) and never
// deletes a directory git doesn't list as a worktree of repoRoot. It only
// prunes worktree metadata when it had to fall back to deleting the
// directory itself — a repo-wide prune would also drop the user's other
// worktrees whose directories are temporarily missing (unmounted volume).
func RemoveWorktree(ctx context.Context, repoRoot, dir string) error {
	dir, err := absWorktreeDir(dir)
	if err != nil {
		return err
	}
	return removeWorktree(ctx, repoRoot, dir, false)
}

// ownDir asserts the caller created dir (AddSparseWorktree's failure path),
// which permits deleting it even when git has no record of it yet.
func removeWorktree(ctx context.Context, repoRoot, dir string, ownDir bool) error {
	if _, err := os.Lstat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("worktree: %w", err)
	}
	if _, err := runGit(ctx, repoRoot, "worktree", "remove", "--force", dir); err == nil {
		return nil
	} else if !ownDir && !isRegisteredWorktree(ctx, repoRoot, dir) {
		return fmt.Errorf("worktree: %s is not a worktree of %s: %w", dir, repoRoot, err)
	}
	// git refused (metadata missing or inconsistent) but the directory is
	// ours: reclaim the disk directly, then drop the dangling metadata.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	_, err := runGit(ctx, repoRoot, "worktree", "prune")
	return err
}

// isRegisteredWorktree reports whether git lists dir as a linked worktree of
// repoRoot.
func isRegisteredWorktree(ctx context.Context, repoRoot, dir string) bool {
	out, err := runGit(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && samePath(p, dir) {
			return true
		}
	}
	return false
}

// samePath compares two paths after symlink resolution (macOS /tmp vs
// /private/tmp).
func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// SparseTreeSize sums the blob sizes at sha for paths matching patterns,
// without checking anything out — a fast estimate of what AddSparseWorktree
// would write to disk (a 31k-entry tree lists in well under 100ms).
func SparseTreeSize(ctx context.Context, repoRoot, sha string, patterns []string) (int64, error) {
	if err := validateSparsePatterns(patterns); err != nil {
		return 0, err
	}
	full, err := resolveCommit(ctx, repoRoot, sha)
	if err != nil {
		return 0, err
	}
	// -z: NUL-terminated records with raw paths, so non-ASCII names are not
	// C-quoted (which would hide them from the pattern match).
	out, err := runGit(ctx, repoRoot, "ls-tree", "-r", "-l", "-z", "--full-tree", full)
	if err != nil {
		return 0, err
	}
	var total int64
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	sc.Split(scanNUL)
	for sc.Scan() {
		// <mode> <type> <sha> <size>\t<path>
		meta, path, ok := strings.Cut(sc.Text(), "\t")
		if !ok || !sparsePatternMatch(path, patterns) {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 4 || fields[1] != "blob" {
			continue
		}
		if n, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
			total += n
		}
	}
	return total, sc.Err()
}

func scanNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// validateSparsePatterns restricts patterns to the subset whose git
// (gitignore-style) semantics sparsePatternMatch reproduces exactly:
// positive globs matched against the basename at any depth. Anchored
// ("/go.mod"), directory-scoped ("cmd/*.go", "**/x"), and negated
// ("!vendor/*.go") patterns would check out one set of files while the size
// estimate counted another, so they are rejected up front.
func validateSparsePatterns(patterns []string) error {
	if len(patterns) == 0 {
		return fmt.Errorf("worktree: no sparse patterns given")
	}
	for _, p := range patterns {
		switch {
		case p == "", strings.ContainsAny(p, "/\\"), strings.HasPrefix(p, "!"), strings.HasPrefix(p, "#"):
			return fmt.Errorf("worktree: unsupported sparse pattern %q (only basename globs like \"*.go\" are supported)", p)
		}
		if _, err := filepath.Match(p, ""); err != nil {
			return fmt.Errorf("worktree: bad sparse pattern %q: %w", p, err)
		}
	}
	return nil
}

func sparsePatternMatch(path string, patterns []string) bool {
	base := path[strings.LastIndex(path, "/")+1:]
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
	}
	return false
}

func absWorktreeDir(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("worktree: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}
	return abs, nil
}
