package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sho-hata/crit/internal/config"
	"github.com/sho-hata/crit/internal/lsp"
	"github.com/sho-hata/crit/internal/session"
	"github.com/sho-hata/crit/internal/vcs"
)

// fakeLSPProvider implements lspProvider without spawning gopls.
type fakeLSPProvider struct {
	hoverContents string
	hoverErr      error
	locations     []lsp.Location
	definitionErr error
	references    []lsp.Location
	referencesErr error
	goroot        string
	gomodcache    string
	shutdowns     int
}

func (f *fakeLSPProvider) Hover(string, int, int) (string, error) {
	return f.hoverContents, f.hoverErr
}

func (f *fakeLSPProvider) Definition(string, int, int) ([]lsp.Location, error) {
	return f.locations, f.definitionErr
}

func (f *fakeLSPProvider) References(string, int, int) ([]lsp.Location, error) {
	return f.references, f.referencesErr
}

func (f *fakeLSPProvider) GoEnv() (string, string) { return f.goroot, f.gomodcache }
func (f *fakeLSPProvider) Shutdown()               { f.shutdowns++ }

// newLSPTestServer builds a test server whose session contains main.go and
// whose LSP provider is the given fake.
func newLSPTestServer(t *testing.T, fake *fakeLSPProvider) (*Server, *Session) {
	t.Helper()
	srv, sess := NewTestServer(t)
	goPath := filepath.Join(sess.RepoRoot, "main.go")
	if err := os.WriteFile(goPath, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess.Files = append(sess.Files, &FileEntry{
		Path: "main.go", AbsPath: goPath, Status: "modified", FileType: "code",
	})
	srv.lsp.binaryAvailable = func() bool { return true }
	srv.lsp.newProvider = func() lspProvider { return fake }
	return srv, sess
}

// newRangeLSPTestServer builds a test server backed by a real git repo, with
// the session in range/PR focus at headSHA. reviewPath is a fresh temp dir
// so syncLSPRoot has somewhere to put the sparse worktree.
func newRangeLSPTestServer(t *testing.T, fake *fakeLSPProvider) (srv *Server, sess *Session, repo, headSHA string) {
	t.Helper()
	repo = vcs.InitTestRepo(t)
	headSHA = vcs.CommitAtForTest(t, repo, "main.go", "package main\n\nfunc main() {}\n", "add main.go")

	sess = &Session{
		Mode:        "git",
		RepoRoot:    repo,
		VCS:         &vcs.GitVCS{},
		ReviewRound: 1,
		Focus:       Focus{Kind: FocusRange, BaseSHA: headSHA, HeadSHA: headSHA},
		Files: []*FileEntry{
			{Path: "main.go", AbsPath: filepath.Join(repo, "main.go"), Status: "added", FileType: "code"},
		},
	}
	sess.InitTestChannels()

	var err error
	srv, err = NewServer(sess, FrontendFS, "", "test", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	srv.reviewPath = t.TempDir()
	srv.lsp.binaryAvailable = func() bool { return true }
	srv.lsp.newProvider = func() lspProvider { return fake }
	return srv, sess, repo, headSHA
}

func doLSPRequest(t *testing.T, srv *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestLSPHoverHappyPath(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "```go\nfunc main()\n```"}
	srv, _ := newLSPTestServer(t, fake)

	w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=3&char=5")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Contents string `json:"contents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Contents != fake.hoverContents {
		t.Errorf("contents = %q, want %q", resp.Contents, fake.hoverContents)
	}
}

func TestLSPProviderReusedAcrossWorkingTreeRequests(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, _ := newLSPTestServer(t, fake)
	created := 0
	srv.lsp.newProvider = func() lspProvider {
		created++
		return fake
	}

	for i := 0; i < 3; i++ {
		w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=3&char=5")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, w.Code, w.Body.String())
		}
	}

	if created != 1 {
		t.Errorf("provider created %d times, want 1", created)
	}
	if fake.shutdowns != 0 {
		t.Errorf("shutdowns = %d, want 0", fake.shutdowns)
	}
}

func TestLSPHoverErrors(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{}
	srv, _ := newLSPTestServer(t, fake)

	tests := []struct {
		name string
		url  string
		code int
	}{
		{"missing path", "/api/lsp/hover?line=1&char=0", http.StatusBadRequest},
		{"non-go file", "/api/lsp/hover?path=test.md&line=1&char=0", http.StatusBadRequest},
		{"bad line", "/api/lsp/hover?path=main.go&line=0&char=0", http.StatusBadRequest},
		{"negative char", "/api/lsp/hover?path=main.go&line=1&char=-1", http.StatusBadRequest},
		{"traversal", "/api/lsp/hover?path=..%2Fmain.go&line=1&char=0", http.StatusBadRequest},
		// Absolute paths are allowed for chained peek jumps but scoped to
		// repo root / GOROOT / GOMODCACHE — anything else is denied.
		{"absolute path outside roots", "/api/lsp/hover?path=%2Fetc%2Fpasswd.go&line=1&char=0", http.StatusForbidden},
		{"file outside root", "/api/lsp/hover?path=missing.go&line=1&char=0", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := doLSPRequest(t, srv, tt.url); w.Code != tt.code {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tt.code, w.Body.String())
			}
		})
	}
}

func TestLSPHoverMethodNotAllowed(t *testing.T) {
	t.Parallel()

	srv, _ := newLSPTestServer(t, &fakeLSPProvider{})
	req := httptest.NewRequest(http.MethodPost, "/api/lsp/hover?path=main.go&line=1&char=0", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestLSPDisabledByConfig(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{}
	srv, _ := newLSPTestServer(t, fake)
	disabled := false
	srv.cfg = Config{LSP: &disabled}

	if w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when lsp disabled", w.Code)
	}
	if srv.lspAvailable() {
		t.Error("lspAvailable must be false when config disables lsp")
	}
}

// TestLSPUnavailableUnderRangeFocus covers range/PR focus without a VCS
// (files mode, or --remote/non-git — see rangeLSPSupported). Range focus
// backed by a local git repo is covered by TestLSPRangeFocusUsesSparseWorktree.
func TestLSPUnavailableUnderRangeFocus(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, sess := newLSPTestServer(t, fake)
	sess.Focus = Focus{Kind: FocusRange, BaseSHA: "b", HeadSHA: "h"}

	if w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 under range focus with no VCS", w.Code)
	}
	if srv.lspAvailable() {
		t.Error("lspAvailable must be false under range/PR focus with no VCS to build a worktree from")
	}
}

// TestLSPUnavailableForRemoteFocus covers --remote range focus: file content
// comes from the GitHub API, so there is no local object store to build a
// sparse worktree from even though sess.VCS is git.
func TestLSPUnavailableForRemoteFocus(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, sess := newLSPTestServer(t, fake)
	sess.VCS = &vcs.GitVCS{}
	sess.RemoteFiles = true
	sess.Focus = Focus{Kind: FocusRange, BaseSHA: "b", HeadSHA: "h"}

	if srv.lspAvailable() {
		t.Error("lspAvailable must be false for --remote range focus even with a git VCS")
	}
}

// TestLSPRangeFocusUsesSparseWorktree covers the happy path: range/PR focus
// backed by a local git repo is available, and an LSP request materializes
// a sparse checkout of Focus.HeadSHA that lspRoot points at (not RepoRoot).
func TestLSPRangeFocusUsesSparseWorktree(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, _, repo, headSHA := newRangeLSPTestServer(t, fake)

	if !srv.lspAvailable() {
		t.Fatal("lspAvailable must be true for range/PR focus backed by a local git repo")
	}

	w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=3&char=5")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	root := srv.lspRoot()
	if root == repo || root == "" {
		t.Fatalf("lspRoot() = %q, want a worktree distinct from RepoRoot %q", root, repo)
	}
	if got := vcs.GitRun(t, root, "rev-parse", "HEAD"); got != headSHA {
		t.Errorf("worktree HEAD = %s, want %s", got, headSHA)
	}
	if _, err := os.Stat(filepath.Join(root, "main.go")); err != nil {
		t.Errorf("main.go missing from lsp worktree: %v", err)
	}
}

// TestLSPWorktreeRebuildsOnFocusSwitch covers the "one worktree per daemon"
// invariant: switching range focus to a different commit must tear down the
// old worktree (and Manager) and rebuild against the new SHA, not silently
// keep serving stale content.
func TestLSPWorktreeRebuildsOnFocusSwitch(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, sess, repo, firstSHA := newRangeLSPTestServer(t, fake)

	if w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0"); w.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, body = %s", w.Code, w.Body.String())
	}
	firstRoot := srv.lspRoot()
	if got := vcs.GitRun(t, firstRoot, "rev-parse", "HEAD"); got != firstSHA {
		t.Fatalf("worktree HEAD = %s, want %s", got, firstSHA)
	}

	secondSHA := vcs.CommitAtForTest(t, repo, "main.go", "package main\n\nfunc main() { println(1) }\n", "change main.go")
	sess.Focus = Focus{Kind: FocusRange, BaseSHA: firstSHA, HeadSHA: secondSHA}

	if w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0"); w.Code != http.StatusOK {
		t.Fatalf("second request: status = %d, body = %s", w.Code, w.Body.String())
	}
	secondRoot := srv.lspRoot()
	// The worktree dir is reused (torn down + rebuilt in place, see
	// syncLSPRoot), so the path is unchanged — it's the *content*
	// that must have moved to secondSHA.
	if secondRoot != firstRoot {
		t.Fatalf("lspRoot() = %q after focus switch, want reused path %q", secondRoot, firstRoot)
	}
	if got := vcs.GitRun(t, secondRoot, "rev-parse", "HEAD"); got != secondSHA {
		t.Errorf("worktree HEAD after focus switch = %s, want %s", got, secondSHA)
	}
	if fake.shutdowns != 1 {
		t.Errorf("shutdowns = %d, want 1 (old Manager torn down on focus switch)", fake.shutdowns)
	}
}

func TestLSPWorktreeDroppedOnSwitchToWorkingTreeFocus(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, sess, repo, _ := newRangeLSPTestServer(t, fake)

	if w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0"); w.Code != http.StatusOK {
		t.Fatalf("range request: status = %d, body = %s", w.Code, w.Body.String())
	}
	worktree := srv.lspRoot()
	if worktree == repo {
		t.Fatalf("lspRoot() = %q, want a worktree distinct from RepoRoot", worktree)
	}

	sess.Focus = Focus{}

	if w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0"); w.Code != http.StatusOK {
		t.Fatalf("working-tree request: status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := srv.lspRoot(); got != repo {
		t.Errorf("lspRoot() = %q after focus clear, want RepoRoot %q", got, repo)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("worktree %s still exists after focus clear (stat err = %v)", worktree, err)
	}
	if fake.shutdowns != 1 {
		t.Errorf("shutdowns = %d, want 1", fake.shutdowns)
	}
}

// TestShutdownLSPRemovesRangeWorktree covers daemon-shutdown cleanup: the
// range-focus worktree must not outlive the daemon.
func TestShutdownLSPRemovesRangeWorktree(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, _, _, _ := newRangeLSPTestServer(t, fake)

	if w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	root := srv.lspRoot()

	srv.ShutdownLSP()

	if _, err := os.Stat(root); err == nil {
		t.Error("lsp worktree still exists after ShutdownLSP")
	}
	if fake.shutdowns != 1 {
		t.Errorf("shutdowns = %d, want 1", fake.shutdowns)
	}
}

// TestLSPWorktreeSizeGuardBlocksOversizedCheckout covers the
// lsp_worktree_max_mb guard: a HeadSHA whose sparse-checkout estimate
// exceeds the configured limit must not be checked out.
func TestLSPWorktreeSizeGuardBlocksOversizedCheckout(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, sess, repo, _ := newRangeLSPTestServer(t, fake)

	big := "package main\n\n// " + strings.Repeat("x", 2*1024*1024) + "\n"
	headSHA := vcs.CommitAtForTest(t, repo, "big.go", big, "add big file")
	sess.Focus = Focus{Kind: FocusRange, BaseSHA: headSHA, HeadSHA: headSHA}
	limitMB := 1
	srv.cfg.LSPWorktreeMaxMB = &limitMB

	w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (size guard), body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "lsp_worktree_max_mb") {
		t.Errorf("body = %q, want a size-limit error mentioning lsp_worktree_max_mb", w.Body.String())
	}
	if srv.lspRoot() != repo {
		t.Errorf("lspRoot() = %q after a blocked checkout, want it to fall back to RepoRoot %q", srv.lspRoot(), repo)
	}
}

// TestLSPWorktreeIdleCleanup covers the idle-shutdown path: a range-focus
// worktree left untouched past the idle timeout must be torn down, along
// with the Manager backed by it, the same way gopls itself idles out.
func TestLSPWorktreeIdleCleanup(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, _, _, _ := newRangeLSPTestServer(t, fake)
	srv.lsp.idleTimeout = 20 * time.Millisecond

	if w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	root := srv.lspRoot()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(root); err == nil {
		t.Error("lsp worktree still exists after the idle timeout")
	}
	if fake.shutdowns != 1 {
		t.Errorf("shutdowns = %d, want 1 (idle timeout must also shut down the Manager)", fake.shutdowns)
	}
}

// TestLSPWorktreeSelfHealsStaleLeftover covers restart-after-crash: a
// worktree directory built by a prior (now-dead) daemon process, which this
// process's in-memory lspState has no record of, must be cleared and
// rebuilt rather than making every LSP request fail with "already exists".
func TestLSPWorktreeSelfHealsStaleLeftover(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, _, repo, headSHA := newRangeLSPTestServer(t, fake)

	dir := session.ReviewPathsFor(srv.reviewPath).LSPWorktree
	if err := vcs.AddSparseWorktree(context.Background(), repo, headSHA, dir, lsp.AllSparsePatterns()); err != nil {
		t.Fatal(err)
	}

	w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := vcs.GitRun(t, srv.lspRoot(), "rev-parse", "HEAD"); got != headSHA {
		t.Errorf("worktree HEAD = %s, want %s", got, headSHA)
	}
}

func TestLSPDefinitionInSessionAndPeek(t *testing.T) {
	t.Parallel()

	srv, sess := newLSPTestServer(t, nil)
	fake := &fakeLSPProvider{
		locations: []lsp.Location{
			{Path: filepath.Join(sess.RepoRoot, "main.go"), Line: 2, Character: 5},
		},
	}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Locations) != 1 {
		t.Fatalf("got %d locations, want 1", len(resp.Locations))
	}
	loc := resp.Locations[0]
	if loc.Path != "main.go" || !loc.InRepo || !loc.InSession {
		t.Errorf("location = %+v; want in-repo in-session main.go", loc)
	}
	if loc.Line != 3 {
		t.Errorf("line = %d, want 3 (1-based)", loc.Line)
	}
	if loc.PeekStart != 1 || len(loc.Peek) == 0 {
		t.Errorf("peek = start %d, %d lines; want start 1 with content", loc.PeekStart, len(loc.Peek))
	}
	if !strings.Contains(strings.Join(loc.Peek, "\n"), "func main()") {
		t.Errorf("peek missing target content: %v", loc.Peek)
	}
}

func TestLSPDefinitionInRepoNotInSession(t *testing.T) {
	t.Parallel()

	srv, sess := newLSPTestServer(t, nil)
	otherPath := filepath.Join(sess.RepoRoot, "helper.go")
	if err := os.WriteFile(otherPath, []byte("package main\n\nfunc helper() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLSPProvider{locations: []lsp.Location{{Path: otherPath, Line: 2}}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if !loc.InRepo || loc.InSession {
		t.Errorf("location = %+v; want in-repo, NOT in-session", loc)
	}
	if len(loc.Peek) == 0 {
		t.Error("in-repo target must carry a peek")
	}
}

func TestLSPDefinitionOutsideAllowedRootsHasNoPeek(t *testing.T) {
	t.Parallel()

	srv, _ := newLSPTestServer(t, nil)
	outside := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(outside, []byte("package secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// goroot/gomodcache empty: the outside dir matches no allowed root.
	fake := &fakeLSPProvider{locations: []lsp.Location{{Path: outside, Line: 0}}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if loc.InRepo || loc.InSession {
		t.Errorf("location = %+v; want outside repo/session", loc)
	}
	if len(loc.Peek) != 0 {
		t.Error("peek must be withheld for paths outside repo/GOROOT/GOMODCACHE")
	}
}

func TestTruncateLineKeepsRuneBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
	}{
		{"ascii", strings.Repeat("a", peekMaxLineLen+100)},
		// Multi-byte runes positioned so a naive byte slice would cut mid-rune.
		{"multibyte", strings.Repeat("あ", peekMaxLineLen)},
		{"mixed", strings.Repeat("a", peekMaxLineLen-1) + strings.Repeat("識", 50)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncateLine(tt.line)
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("long line must end with ellipsis, got %q…", got[len(got)-10:])
			}
			if !utf8.ValidString(got) {
				t.Error("truncation split a multi-byte rune")
			}
			if len(got) > peekMaxLineLen+len("…") {
				t.Errorf("truncated to %d bytes, want <= %d", len(got), peekMaxLineLen+len("…"))
			}
		})
	}
	short := "short あいう line"
	if got := truncateLine(short); got != short {
		t.Errorf("short line must be unchanged, got %q", got)
	}
}

func TestReadPeekBoundaries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name string, lineCount int) string {
		path := filepath.Join(dir, name)
		var b strings.Builder
		for i := 0; i < lineCount; i++ {
			fmt.Fprintf(&b, "line %d\n", i+1)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// A trailing newline must not produce a phantom empty line past EOF.
	small := write("small.go", 3)
	start, lines, truncated := readPeek(small, 2, peekFullFileMaxLines, peekContextLines)
	if start != 1 || truncated || len(lines) != 3 {
		t.Errorf("small file: start=%d truncated=%v lines=%d, want 1/false/3", start, truncated, len(lines))
	}

	// Exactly peekFullFileMaxLines lines (newline-terminated) is still a
	// whole-file peek, not a truncated window.
	exact := write("exact.go", peekFullFileMaxLines)
	start, lines, truncated = readPeek(exact, 1000, peekFullFileMaxLines, peekContextLines)
	if start != 1 || truncated || len(lines) != peekFullFileMaxLines {
		t.Errorf("exact-limit file: start=%d truncated=%v lines=%d, want 1/false/%d",
			start, truncated, len(lines), peekFullFileMaxLines)
	}

	// One line over the limit tips it into the ±peekContextLines window.
	over := write("over.go", peekFullFileMaxLines+1)
	start, lines, truncated = readPeek(over, 1000, peekFullFileMaxLines, peekContextLines)
	if !truncated || start != 1000-peekContextLines || len(lines) != 2*peekContextLines+1 {
		t.Errorf("over-limit file: start=%d truncated=%v lines=%d, want %d/true/%d",
			start, truncated, len(lines), 1000-peekContextLines, 2*peekContextLines+1)
	}
}

func TestLSPDefinitionPeekTruncatesLargeFiles(t *testing.T) {
	t.Parallel()

	srv, sess := newLSPTestServer(t, nil)
	bigPath := filepath.Join(sess.RepoRoot, "generated.go")
	var b strings.Builder
	b.WriteString("package main\n")
	for i := 0; i < 3000; i++ {
		b.WriteString("// filler line\n")
	}
	if err := os.WriteFile(bigPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLSPProvider{locations: []lsp.Location{{Path: bigPath, Line: 1500}}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if !loc.PeekTruncated {
		t.Error("large file peek must set peek_truncated")
	}
	if len(loc.Peek) != 2*100+1 {
		t.Errorf("windowed peek = %d lines, want %d", len(loc.Peek), 2*100+1)
	}
	if loc.PeekStart != 1501-100 {
		t.Errorf("peek_start = %d, want %d", loc.PeekStart, 1501-100)
	}

	// Small files come back whole and untruncated. Fresh response struct:
	// peek_truncated is omitempty, and json.Unmarshal into a reused struct
	// would keep the previous true.
	fake.locations = []lsp.Location{{Path: filepath.Join(sess.RepoRoot, "main.go"), Line: 0}}
	w = doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp2 struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.Locations[0].PeekTruncated {
		t.Error("small file peek must not be truncated")
	}
	if resp2.Locations[0].PeekStart != 1 {
		t.Errorf("small file peek_start = %d, want 1 (whole file)", resp2.Locations[0].PeekStart)
	}
}

func TestLSPDefinitionGorootPeek(t *testing.T) {
	t.Parallel()

	srv, _ := newLSPTestServer(t, nil)
	goroot := t.TempDir()
	stdlibFile := filepath.Join(goroot, "src", "fmt", "print.go")
	if err := os.MkdirAll(filepath.Dir(stdlibFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stdlibFile, []byte("package fmt\n\nfunc Println() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLSPProvider{
		locations: []lsp.Location{{Path: stdlibFile, Line: 2}},
		goroot:    goroot,
	}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if loc.InRepo {
		t.Errorf("stdlib target must not be in-repo: %+v", loc)
	}
	if loc.DisplayPath != "$GOROOT/src/fmt/print.go" {
		t.Errorf("display_path = %q", loc.DisplayPath)
	}
	if len(loc.Peek) == 0 {
		t.Error("GOROOT target must carry a peek")
	}
}

// TestLSPAbsolutePathScoping covers chained jumps from the peek popup:
// absolute paths are accepted only under repo root / GOROOT / GOMODCACHE.
func TestLSPAbsolutePathScoping(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, sess := newLSPTestServer(t, fake)

	goroot := t.TempDir()
	stdlibFile := filepath.Join(goroot, "src", "fmt", "print.go")
	if err := os.MkdirAll(filepath.Dir(stdlibFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stdlibFile, []byte("package fmt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.goroot = goroot

	outside := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(outside, []byte("package secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		code int
	}{
		{"absolute in repo", filepath.Join(sess.RepoRoot, "main.go"), http.StatusOK},
		{"absolute in GOROOT", stdlibFile, http.StatusOK},
		{"absolute outside allowed roots", outside, http.StatusForbidden},
		{"absolute traversal into repo stays allowed after Clean", sess.RepoRoot + "/sub/../main.go", http.StatusOK},
		{"absolute traversal escaping repo", sess.RepoRoot + "/../escape.go", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doLSPRequest(t, srv, "/api/lsp/hover?path="+url.QueryEscape(tt.path)+"&line=1&char=0")
			if w.Code != tt.code {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tt.code, w.Body.String())
			}
		})
	}
}

// lspReferencesResponse mirrors the /api/lsp/references payload in tests.
type lspReferencesResponse struct {
	Locations []lspLocationResponse `json:"locations"`
	Truncated bool                  `json:"truncated"`
}

func TestLSPReferencesSortedWithSnippetPeek(t *testing.T) {
	t.Parallel()

	srv, sess := newLSPTestServer(t, nil)
	helperPath := filepath.Join(sess.RepoRoot, "helper.go")
	if err := os.WriteFile(helperPath, []byte("package main\n\nfunc helper() { main() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// helper.go sorts first alphabetically but is not part of the review, so
	// the in-session main.go must come first.
	fake := &fakeLSPProvider{references: []lsp.Location{
		{Path: helperPath, Line: 2, Character: 16},
		{Path: filepath.Join(sess.RepoRoot, "main.go"), Line: 2, Character: 5},
	}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=3&char=6")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp lspReferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Truncated {
		t.Error("truncated must be false for a small reference list")
	}
	if len(resp.Locations) != 2 {
		t.Fatalf("got %d locations, want 2", len(resp.Locations))
	}
	if resp.Locations[0].Path != "main.go" || resp.Locations[1].Path != "helper.go" {
		t.Errorf("in-session file must sort first, got %q then %q",
			resp.Locations[0].Path, resp.Locations[1].Path)
	}
	ref := resp.Locations[0]
	if !ref.InRepo || !ref.InSession || ref.Line != 3 {
		t.Errorf("main.go reference = %+v; want in-repo in-session line 3", ref)
	}
	// Small files come back whole; the reference's own line must be inside
	// the peek window so the UI can render a snippet row.
	idx := ref.Line - ref.PeekStart
	if idx < 0 || idx >= len(ref.Peek) {
		t.Fatalf("reference line %d outside peek window start=%d len=%d", ref.Line, ref.PeekStart, len(ref.Peek))
	}
	if !strings.Contains(ref.Peek[idx], "func main()") {
		t.Errorf("snippet = %q, want the reference line", ref.Peek[idx])
	}
}

// TestLSPReferencesCapKeepsRelevantFiles covers the interaction between the
// relevance sort and the maxReferenceLocations cap: when more references
// exist than fit, the ones in files under review must survive even though
// their paths sort last alphabetically.
func TestLSPReferencesCapKeepsRelevantFiles(t *testing.T) {
	t.Parallel()

	srv, sess := newLSPTestServer(t, nil)
	// aaa.go is in the repo but not under review, and sorts before main.go.
	bulkPath := filepath.Join(sess.RepoRoot, "aaa.go")
	if err := os.WriteFile(bulkPath, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs := make([]lsp.Location, 0, maxReferenceLocations+10)
	for i := 0; i < maxReferenceLocations+5; i++ {
		refs = append(refs, lsp.Location{Path: bulkPath, Line: i})
	}
	refs = append(refs, lsp.Location{Path: filepath.Join(sess.RepoRoot, "main.go"), Line: 2})
	fake := &fakeLSPProvider{references: refs}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=3&char=6")
	var resp lspReferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || len(resp.Locations) != maxReferenceLocations {
		t.Fatalf("truncated=%v with %d locations, want true with %d",
			resp.Truncated, len(resp.Locations), maxReferenceLocations)
	}
	if resp.Locations[0].Path != "main.go" {
		t.Errorf("first location = %q, want the in-session main.go to survive the cap",
			resp.Locations[0].Path)
	}
}

// TestReferenceCharacterTiebreak pins the character tiebreak: two references
// on the same line must have a deterministic order.
func TestReferenceCharacterTiebreak(t *testing.T) {
	t.Parallel()

	_, sess := newLSPTestServer(t, nil)
	path := filepath.Join(sess.RepoRoot, "main.go")
	locs := []lsp.Location{
		{Path: path, Line: 4, Character: 20},
		{Path: path, Line: 4, Character: 3},
	}
	rc := newRootCache(sess.RepoRoot, "", "")
	sortReferences(locs, sess, rc)
	if locs[0].Character != 3 || locs[1].Character != 20 {
		t.Errorf("same-line references not ordered by character: %+v", locs)
	}
}

func TestLSPReferencesSmallPeekWindow(t *testing.T) {
	t.Parallel()

	srv, sess := newLSPTestServer(t, nil)
	bigPath := filepath.Join(sess.RepoRoot, "big.go")
	var b strings.Builder
	b.WriteString("package main\n")
	for i := 0; i < 100; i++ {
		b.WriteString("// filler line\n")
	}
	if err := os.WriteFile(bigPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLSPProvider{references: []lsp.Location{{Path: bigPath, Line: 50}}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=1&char=0")
	var resp lspReferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if !loc.PeekTruncated {
		t.Error("reference peek in a >50-line file must be windowed")
	}
	if len(loc.Peek) != 2*10+1 {
		t.Errorf("reference peek = %d lines, want %d (±10 window)", len(loc.Peek), 2*10+1)
	}
}

func TestLSPReferencesTruncatesLongLists(t *testing.T) {
	t.Parallel()

	srv, sess := newLSPTestServer(t, nil)
	mainPath := filepath.Join(sess.RepoRoot, "main.go")
	refs := make([]lsp.Location, maxReferenceLocations+50)
	for i := range refs {
		refs[i] = lsp.Location{Path: mainPath, Line: i % 3}
	}
	fake := &fakeLSPProvider{references: refs}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=1&char=0")
	var resp lspReferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated {
		t.Error("truncated must be true when the list is capped")
	}
	if len(resp.Locations) != maxReferenceLocations {
		t.Errorf("got %d locations, want %d", len(resp.Locations), maxReferenceLocations)
	}
}

func TestLSPReferencesProviderError(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{referencesErr: os.ErrDeadlineExceeded}
	srv, _ := newLSPTestServer(t, fake)

	if w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=1&char=0"); w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestLSPReferencesMethodNotAllowed(t *testing.T) {
	t.Parallel()

	srv, _ := newLSPTestServer(t, &fakeLSPProvider{})
	req := httptest.NewRequest(http.MethodPost, "/api/lsp/references?path=main.go&line=1&char=0", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestLSPConfigExposesAvailability(t *testing.T) {
	t.Parallel()

	srv, _ := newLSPTestServer(t, &fakeLSPProvider{})
	w := doLSPRequest(t, srv, "/api/config")
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["lsp_available"] != true {
		t.Errorf("lsp_available = %v, want true", resp["lsp_available"])
	}
}

func TestShutdownLSPStopsProvider(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{}
	srv, _ := newLSPTestServer(t, fake)
	// Provider is created lazily; trigger it, then shut down.
	doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0")
	srv.ShutdownLSP()
	if fake.shutdowns != 1 {
		t.Errorf("shutdowns = %d, want 1", fake.shutdowns)
	}
	// Idempotent when nothing is running.
	srv.ShutdownLSP()
	if fake.shutdowns != 1 {
		t.Errorf("shutdowns after second call = %d, want 1", fake.shutdowns)
	}
}

func TestLSPEnabledConfigDefault(t *testing.T) {
	t.Parallel()

	var c config.Config
	if !c.LSPEnabled() {
		t.Error("LSPEnabled must default to true")
	}
}

// TestLSPManagerDefaultProviderNoDeadlock covers the real provider branch of
// lspManager (newProvider unset), which resolves the workspace root while
// already holding s.lsp.mu. Every other test injects a fake provider and so
// never enters that branch — which is how a self-deadlock there (routing
// through lspRoot, which takes the same non-reentrant mutex, hanging every
// LSP request for the daemon's lifetime) once shipped. lsp.NewManager does
// not spawn gopls, so this needs no binary on PATH.
func TestLSPManagerDefaultProviderNoDeadlock(t *testing.T) {
	t.Parallel()

	srv, sess := newLSPTestServer(t, nil)
	srv.lsp.newProvider = nil

	done := make(chan lspProvider, 1)
	go func() { done <- srv.lspManager() }()
	select {
	case prov := <-done:
		if prov == nil {
			t.Fatal("lspManager returned a nil provider")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lspManager deadlocked resolving the workspace root")
	}
	if got := srv.lspRoot(); got != sess.RepoRoot {
		t.Fatalf("lspRoot = %q, want %q", got, sess.RepoRoot)
	}
	srv.ShutdownLSP()
}

// TestLSPHoverTypeScriptFile covers multi-language support: the endpoints
// accept any extension a registered language server covers, not just .go.
func TestLSPHoverTypeScriptFile(t *testing.T) {
	t.Parallel()

	fake := &fakeLSPProvider{hoverContents: "```ts\nconst x: number\n```"}
	srv, sess := newLSPTestServer(t, fake)
	tsPath := filepath.Join(sess.RepoRoot, "app.ts")
	if err := os.WriteFile(tsPath, []byte("const x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess.Files = append(sess.Files, &FileEntry{
		Path: "app.ts", AbsPath: tsPath, Status: "modified", FileType: "code",
	})

	w := doLSPRequest(t, srv, "/api/lsp/hover?path=app.ts&line=1&char=6")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Contents string `json:"contents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Contents != fake.hoverContents {
		t.Errorf("contents = %q, want %q", resp.Contents, fake.hoverContents)
	}
}

// TestLSPExtensions covers the /api/config extension list: all registered
// extensions when the availability hook reports servers installed, nil when
// LSP is unavailable.
func TestLSPExtensions(t *testing.T) {
	t.Parallel()

	srv, _ := newLSPTestServer(t, &fakeLSPProvider{})
	exts := srv.lspExtensions()
	want := map[string]bool{"go": false, "ts": false, "tsx": false, "js": false}
	for _, e := range exts {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for e, seen := range want {
		if !seen {
			t.Errorf("lspExtensions() = %v, missing %q", exts, e)
		}
	}

	srv.lsp.binaryAvailable = func() bool { return false }
	if got := srv.lspExtensions(); got != nil {
		t.Errorf("lspExtensions() with no servers = %v, want nil", got)
	}
}
