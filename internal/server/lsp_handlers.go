package server

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sho-hata/crit/internal/lsp"
	"github.com/sho-hata/crit/internal/pathsafe"
	"github.com/sho-hata/crit/internal/session"
	"github.com/sho-hata/crit/internal/vcs"
)

// lspSparsePatterns returns the sparse-checkout pattern set for the
// range-focus LSP worktree: source and project files of every language that
// both covers a file in the session and has its server installed — enough
// for the servers without the rest of the tree, and without inflating the
// checkout (or its lsp_worktree_max_mb estimate) with languages the review
// doesn't contain.
func (s *Server) lspSparsePatterns(sess *Session) []string {
	paths := make([]string, 0, len(sess.Files))
	for _, f := range sess.Files {
		paths = append(paths, f.Path)
	}
	return lsp.SparsePatternsForFiles(paths, s.lspLangAvailable())
}

// peekFullFileMaxLines is the largest file sent to the peek popup in full.
// Above this (huge generated code in the module cache can reach tens of
// thousands of lines) the peek falls back to a ±peekContextLines window so
// neither the JSON payload nor the frontend's per-line highlighting balloons.
const peekFullFileMaxLines = 2000

// peekContextLines is how many lines of context a windowed peek carries on
// each side of the target line when the file is too large to send in full.
const peekContextLines = 100

// peekMaxLineLen truncates pathological lines (minified/generated code) in
// peek payloads.
const peekMaxLineLen = 500

// References can return many locations, so each carries a much smaller peek
// window than a definition (the list row shows one line; the window only
// feeds the click-to-peek popup) and the total count is capped.
const (
	refPeekFullFileMaxLines = 50
	refPeekContextLines     = 10
	maxReferenceLocations   = 200
)

// lspProvider is the slice of lsp.Manager the handlers need; an interface so
// tests can inject a fake without spawning gopls.
type lspProvider interface {
	Hover(absPath string, line, character int) (string, error)
	Definition(absPath string, line, character int) ([]lsp.Location, error)
	References(absPath string, line, character int) ([]lsp.Location, error)
	PeekRoots() []lsp.PeekRoot
	Shutdown()
}

// lspState holds the lazily-created LSP manager and, for range/PR focus, the
// sparse worktree backing it. Lives on Server via composition (see server.go).
type lspState struct {
	mu   sync.Mutex
	prov lspProvider
	// worktreeDir is the sparse checkout prov is rooted at when the session
	// is in range/PR focus; empty when prov (if any) is rooted at the
	// working tree (sess.RepoRoot).
	worktreeDir string
	// worktreeSHA is the Focus.HeadSHA worktreeDir was built from, used to
	// detect a focus switch to a different commit.
	worktreeSHA string
	// worktreePatterns is the sparse pattern set worktreeDir was built with.
	// Patterns depend on which servers are installed, which can change while
	// a daemon runs (user installs typescript-language-server mid-session),
	// so SHA equality alone is not enough to reuse a checkout.
	worktreePatterns []string
	// idleTimer drops worktreeDir (and prov) after lsp.DefaultIdleTimeout
	// without an LSP request, matching gopls's own idle shutdown so a
	// quietly-abandoned range-focus review doesn't keep a checkout on disk.
	idleTimer *time.Timer
	// idleTimeout overrides lsp.DefaultIdleTimeout in tests.
	idleTimeout time.Duration
	// newProvider creates the provider on first use; tests override it.
	newProvider func() lspProvider
	// langAvailable overrides the per-language server PATH lookup in tests.
	// Unlike a single boolean, a per-language predicate can express mixed
	// machines ("gopls yes, typescript-language-server no"), which is
	// exactly the per-language activation behavior worth testing.
	langAvailable func(*lsp.Language) bool
}

// lspAvailable reports whether LSP features should be offered to the
// frontend: enabled in config, at least one language server installed, and a
// repo root to anchor the workspace.
func (s *Server) lspAvailable() bool {
	if !s.cfg.LSPEnabled() {
		return false
	}
	sess := s.session.Load()
	if sess == nil || sess.RepoRoot == "" {
		return false
	}
	if sess.Focus.Kind == FocusRange && !rangeLSPSupported(sess) {
		return false
	}
	return lsp.Any(s.lspLangAvailable())
}

// lspLangAvailable returns the predicate deciding whether a language's
// server is installed: the test hook when set, the real PATH lookup
// otherwise.
func (s *Server) lspLangAvailable() func(*lsp.Language) bool {
	if s.lsp.langAvailable != nil {
		return s.lsp.langAvailable
	}
	return (*lsp.Language).Available
}

// lspExtensions returns the file extensions (no dots) LSP features cover:
// the extensions of every language whose server is installed, or nil when
// LSP is unavailable. Sent to the frontend via /api/config so it only offers
// hover/definition on files the server can actually answer for.
func (s *Server) lspExtensions() []string {
	if !s.lspAvailable() {
		return nil
	}
	return lsp.Extensions(s.lspLangAvailable())
}

func rangeLSPSupported(sess *Session) bool {
	if sess.RemoteFiles {
		return false
	}
	return sess.VCS != nil && sess.VCS.Name() == "git"
}

// lspRoot returns the directory LSP features operate on: the workspace the
// language server is anchored at, the base for repo-relative request paths,
// the root peek reads are authorized against, and the base used to map
// server result paths back to repo-relative paths for the frontend. Every
// LSP code path must go through this rather than sess.RepoRoot so that the
// workspace can be pointed somewhere other than the working tree (a checkout
// of Focus.HeadSHA for range/PR focus) without the pieces drifting apart.
func (s *Server) lspRoot() string {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	return s.lspRootLocked()
}

// lspRootLocked is lspRoot for callers that already hold s.lsp.mu. sync.Mutex
// is not reentrant, so a locked caller (lspManager) must never route through
// lspRoot — that self-deadlocks the request and leaves the mutex held for the
// daemon's lifetime.
func (s *Server) lspRootLocked() string {
	sess := s.session.Load()
	if sess == nil {
		return ""
	}
	if s.lsp.worktreeDir != "" {
		return s.lsp.worktreeDir
	}
	return sess.RepoRoot
}

// repoRootLocked returns the session's working tree, or "" when there is no
// session yet. Caller holds s.lsp.mu (see lspRootLocked).
func (s *Server) repoRootLocked() string {
	sess := s.session.Load()
	if sess == nil {
		return ""
	}
	return sess.RepoRoot
}

// syncLSPRoot points lspRoot at content matching what the reviewer sees.
// gopls reads whatever is on disk, which is correct for the normal
// working-tree focus (sess.RepoRoot). But in range/PR focus the review pane
// shows each file as it was at Focus.HeadSHA, which can differ from the
// current working tree — so rather than let gopls answer against the wrong
// content, this checks out HeadSHA into a throwaway git worktree and points
// LSP at that instead. Called at the top of every LSP request, before
// lspRoot/lspManager are read.
func (s *Server) syncLSPRoot() error {
	sess := s.session.Load()
	if sess == nil {
		return fmt.Errorf("session not ready")
	}

	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()

	if sess.Focus.Kind != FocusRange {
		if s.lsp.worktreeDir != "" {
			s.dropLSPRootLocked(sess)
		}
		return nil
	}
	patterns := s.lspSparsePatterns(sess)
	if s.lsp.worktreeDir != "" && s.lsp.worktreeSHA == sess.Focus.HeadSHA &&
		slices.Equal(s.lsp.worktreePatterns, patterns) {
		s.touchLSPIdleLocked()
		return nil // already rooted at this commit with the same pattern set
	}

	// Rebuild lazily, here on the first LSP request against the new SHA,
	// rather than reacting to every focus change immediately — a review may
	// switch PRs several times before anyone actually hovers a symbol.
	s.dropLSPRootLocked(sess)

	dir := session.ReviewPathsFor(s.reviewPath).LSPWorktree
	if _, err := os.Lstat(dir); err == nil {
		// dropLSPRootLocked just ran and found no worktree of its own to
		// remove, yet the directory exists — left behind by a daemon that
		// crashed before it could clean up. AddSparseWorktree refuses an
		// existing dir, so clear it before building fresh.
		if err := vcs.RemoveWorktree(s.shutdownCtx, sess.RepoRoot, dir); err != nil {
			return fmt.Errorf("clearing stale lsp worktree: %w", err)
		}
	}

	if limitMB := s.cfg.LSPWorktreeSizeLimitMB(); limitMB > 0 {
		size, err := vcs.SparseTreeSize(s.shutdownCtx, sess.RepoRoot, sess.Focus.HeadSHA, patterns)
		if err != nil {
			return fmt.Errorf("estimating lsp worktree size: %w", err)
		}
		if limitBytes := int64(limitMB) * 1024 * 1024; size > limitBytes {
			return fmt.Errorf("lsp worktree would be %dMB, over the %dMB lsp_worktree_max_mb limit", size/(1024*1024), limitMB)
		}
	}

	if err := vcs.AddSparseWorktree(s.shutdownCtx, sess.RepoRoot, sess.Focus.HeadSHA, dir, patterns); err != nil {
		return fmt.Errorf("preparing lsp worktree: %w", err)
	}
	s.lsp.worktreeDir = dir
	s.lsp.worktreeSHA = sess.Focus.HeadSHA
	s.lsp.worktreePatterns = patterns
	s.touchLSPIdleLocked()
	return nil
}

// touchLSPIdleLocked (re)arms the timer that drops the range-focus worktree
// after lsp.DefaultIdleTimeout of inactivity. Caller holds s.lsp.mu.
func (s *Server) touchLSPIdleLocked() {
	if s.lsp.idleTimer != nil {
		s.lsp.idleTimer.Stop()
	}
	timeout := s.lsp.idleTimeout
	if timeout == 0 {
		timeout = lsp.DefaultIdleTimeout
	}
	s.lsp.idleTimer = time.AfterFunc(timeout, s.dropIdleLSPRoot)
}

// dropIdleLSPRoot is the idleTimer callback: it runs unlocked (a fresh
// goroutine, not holding s.lsp.mu), so it takes the lock itself before
// touching lsp state.
func (s *Server) dropIdleLSPRoot() {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	s.dropLSPRootLocked(s.session.Load())
}

// dropLSPRootLocked shuts down the provider and removes the worktree
// backing it, if any, for syncLSPRoot (rebuilding for a new commit) and
// ShutdownLSP alike. A failed removal is only logged: ShutdownLSP ignores
// the error regardless, and on the syncLSPRoot path a leftover worktree
// just fails the AddSparseWorktree right after this call — surfacing on
// its own. Caller holds s.lsp.mu.
func (s *Server) dropLSPRootLocked(sess *Session) {
	if s.lsp.idleTimer != nil {
		s.lsp.idleTimer.Stop()
		s.lsp.idleTimer = nil
	}
	if s.lsp.prov != nil {
		s.lsp.prov.Shutdown()
		s.lsp.prov = nil
	}
	if s.lsp.worktreeDir == "" {
		return
	}
	dir := s.lsp.worktreeDir
	s.lsp.worktreeDir = ""
	s.lsp.worktreeSHA = ""
	s.lsp.worktreePatterns = nil
	if sess == nil || sess.RepoRoot == "" {
		return
	}
	if err := vcs.RemoveWorktree(s.shutdownCtx, sess.RepoRoot, dir); err != nil {
		log.Printf("lsp: removing stale worktree %s: %v", dir, err)
	}
}

// lspManager returns the shared LSP provider, creating it on first call.
// gopls itself is spawned even later — on the first LSP request inside the
// manager (lazy start keeps parallel worktree daemons cheap). Callers on the
// request path must call syncLSPRoot first (see lspRoot).
func (s *Server) lspManager() lspProvider {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	if s.lsp.prov == nil {
		if s.lsp.newProvider != nil {
			s.lsp.prov = s.lsp.newProvider()
		} else {
			// shutdownCtx bounds the gopls subprocess: SIGINT/SIGTERM on the
			// daemon kills it instead of leaking.
			// The second root is the working tree: when the first is a
			// range/PR focus worktree, git-untracked dependencies live only
			// in the real checkout (see Manager.depRoot).
			s.lsp.prov = lsp.NewManager(s.lspRootLocked(), s.repoRootLocked(), s.shutdownCtx)
		}
	}
	return s.lsp.prov
}

// ShutdownLSP stops the language server if one was started, and removes the
// range-focus worktree backing it, if any. Called on daemon shutdown.
func (s *Server) ShutdownLSP() {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	s.dropLSPRootLocked(s.session.Load())
}

// parseLSPParams validates the shared query parameters of the LSP endpoints
// and resolves the repo-relative path to an absolute one. line is 1-based
// (matching the UI's NewNum); char is a 0-based UTF-16 offset which is passed
// through to the LSP server verbatim (LSP's default encoding is UTF-16, and
// the browser's JS strings are natively UTF-16 — no conversion needed).
func (s *Server) parseLSPParams(w http.ResponseWriter, r *http.Request) (absPath string, line0, char int, ok bool) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return "", 0, 0, false
	}
	if !s.lspAvailable() {
		http.Error(w, "LSP not available", http.StatusNotFound)
		return "", 0, 0, false
	}
	if err := s.syncLSPRoot(); err != nil {
		http.Error(w, fmt.Sprintf("lsp workspace: %v", err), http.StatusBadGateway)
		return "", 0, 0, false
	}
	q := r.URL.Query()
	reqPath := q.Get("path")
	lang := lsp.LanguageForPath(reqPath)
	if reqPath == "" || lang == nil {
		http.Error(w, "no language server covers this file type", http.StatusBadRequest)
		return "", 0, 0, false
	}
	// A registered but uninstalled language is a client error ("unsupported
	// here"), not an upstream failure — without this check the request would
	// reach startServer and surface as a misleading 502.
	if !s.lspLangAvailable()(lang) {
		http.Error(w, "no language server installed for this file type", http.StatusBadRequest)
		return "", 0, 0, false
	}
	line, err := strconv.Atoi(q.Get("line"))
	if err != nil || line < 1 {
		http.Error(w, "line must be a positive integer", http.StatusBadRequest)
		return "", 0, 0, false
	}
	char, err = strconv.Atoi(q.Get("char"))
	if err != nil || char < 0 {
		http.Error(w, "char must be a non-negative integer", http.StatusBadRequest)
		return "", 0, 0, false
	}
	absPath, ok = s.resolveLSPRequestPath(w, reqPath)
	if !ok {
		return "", 0, 0, false
	}
	return absPath, line - 1, char, true
}

// resolveLSPRequestPath resolves an LSP request's path parameter to an
// absolute path under an allowed root, writing the HTTP error on failure.
func (s *Server) resolveLSPRequestPath(w http.ResponseWriter, reqPath string) (string, bool) {
	root := s.lspRoot()

	if filepath.IsAbs(reqPath) {
		// Absolute paths support chained jumps from the peek popup. They are
		// accepted ONLY under the same roots the peek itself may read (the
		// LSP root plus the installed languages' extra roots) — this endpoint
		// must not become a general filesystem probe.
		absPath := filepath.Clean(reqPath)
		if !s.lspPathAllowed(absPath, root) {
			http.Error(w, "Access denied", http.StatusForbidden)
			return "", false
		}
		return absPath, true
	}

	cleaned := filepath.ToSlash(filepath.Clean(reqPath))
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		http.Error(w, "Invalid file path", http.StatusBadRequest)
		return "", false
	}
	absPath := filepath.Join(root, filepath.FromSlash(cleaned))
	if !pathWithinRoot(absPath, root) {
		http.Error(w, "Access denied", http.StatusForbidden)
		return "", false
	}
	return absPath, true
}

// classifyRoot results: rootRepo for the LSP root, an index >= 0 into the
// extra-roots slice (GOROOT, GOMODCACHE, the global node_modules, …), or
// rootNone.
const (
	rootNone = -2
	rootRepo = -1
)

// rootCache memoizes classifyRoot per path for one request. References
// return many locations concentrated in few files, and each classification
// resolves symlinks against every allowed root.
type rootCache struct {
	root   string
	extras []lsp.PeekRoot
	seen   map[string]int
}

func newRootCache(root string, extras []lsp.PeekRoot) *rootCache {
	return &rootCache{
		root:   root,
		extras: extras,
		seen:   make(map[string]int),
	}
}

// relPath maps an absolute path under the LSP root to the slash-separated
// repo-relative form the frontend and Session.FileByPath use.
func (c *rootCache) relPath(absPath string) (string, bool) {
	rel, err := filepath.Rel(c.root, absPath)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func (c *rootCache) classify(absPath string) int {
	if kind, ok := c.seen[absPath]; ok {
		return kind
	}
	kind := classifyRoot(absPath, c.root, c.extras)
	c.seen[absPath] = kind
	return kind
}

// classifyRoot resolves absPath against the roots LSP features may touch —
// the LSP root plus each installed language's extra roots (GOROOT,
// GOMODCACHE, the global node_modules) — and reports which one contains it.
// This is the single source of truth for authorization (lspPathAllowed),
// peek readability, and display formatting — the classification must never
// drift between those uses.
func classifyRoot(absPath, root string, extras []lsp.PeekRoot) int {
	if pathWithinRoot(absPath, root) {
		return rootRepo
	}
	for i, ex := range extras {
		if ex.Path != "" && pathWithinRoot(absPath, ex.Path) {
			return i
		}
	}
	return rootNone
}

// lspPathAllowed reports whether an absolute path lies under one of the
// roots LSP features may touch: the LSP root or an installed language's
// extra roots (GOROOT, GOMODCACHE, the global node_modules, Python's stdlib,
// site-packages and pyright's typeshed).
func (s *Server) lspPathAllowed(absPath, root string) bool {
	return classifyRoot(absPath, root, s.lspManager().PeekRoots()) != rootNone
}

// handleLSPHover returns hover documentation for a position.
// GET /api/lsp/hover?path=internal/foo.go&line=42&char=13
func (s *Server) handleLSPHover(w http.ResponseWriter, r *http.Request) {
	absPath, line0, char, ok := s.parseLSPParams(w, r)
	if !ok {
		return
	}
	contents, err := s.lspManager().Hover(absPath, line0, char)
	if err != nil {
		http.Error(w, fmt.Sprintf("lsp hover: %v", err), http.StatusBadGateway)
		return
	}
	resp := map[string]any{"contents": contents}
	if s.lspLocalEnv(absPath) {
		resp["local_env"] = true
	}
	writeJSON(w, resp)
}

// lspLocalEnv reports whether an answer about absPath should carry the
// local-environment note: the reviewer is looking at a SHA (range/PR focus)
// and the file's language resolves third-party packages against their own
// environment, which is not that SHA's. In working-tree focus the code under
// review and the environment are the same tree, so there is nothing to say.
func (s *Server) lspLocalEnv(absPath string) bool {
	sess := s.session.Load()
	if sess == nil || sess.Focus.Kind != FocusRange {
		return false
	}
	lang := lsp.LanguageForPath(absPath)
	return lang != nil && lang.LocalEnv
}

// lspLocationResponse is one definition target sent to the frontend.
type lspLocationResponse struct {
	// Path is repo-relative (slash-separated) when InRepo, absolute otherwise.
	Path        string   `json:"path"`
	DisplayPath string   `json:"display_path"`
	Line        int      `json:"line"` // 1-based
	InSession   bool     `json:"in_session"`
	InRepo      bool     `json:"in_repo"`
	PeekStart   int      `json:"peek_start,omitempty"` // 1-based first line of Peek
	Peek        []string `json:"peek,omitempty"`
	// PeekTruncated is true when the file was too large to send in full and
	// Peek is a ±peekContextLines window instead.
	PeekTruncated bool `json:"peek_truncated,omitempty"`
	// LocalEnv is true when the target sits in the reviewer's own installed
	// packages while a range/PR focus is showing a different SHA: the source
	// shown is their local copy, which can differ from what the PR uses.
	LocalEnv bool `json:"local_env,omitempty"`
}

// handleLSPDefinition returns definition locations for a position, each with
// an inline peek so the frontend can always render something — including when
// the target line is outside the visible diff.
// GET /api/lsp/definition?path=internal/foo.go&line=42&char=13
func (s *Server) handleLSPDefinition(w http.ResponseWriter, r *http.Request) {
	absPath, line0, char, ok := s.parseLSPParams(w, r)
	if !ok {
		return
	}
	mgr := s.lspManager()
	locations, err := mgr.Definition(absPath, line0, char)
	if err != nil {
		http.Error(w, fmt.Sprintf("lsp definition: %v", err), http.StatusBadGateway)
		return
	}
	sess := s.session.Load()
	rc := newRootCache(s.lspRoot(), mgr.PeekRoots())
	resp := make([]lspLocationResponse, 0, len(locations))
	for _, loc := range locations {
		resp = append(resp, resolveLocation(sess, loc, peekFullFileMaxLines, peekContextLines, rc))
	}
	writeJSON(w, map[string]any{"locations": resp})
}

// handleLSPReferences returns all reference locations for a position
// (declaration included), sorted by path and line for stable per-file
// grouping in the UI. Each location carries a small peek window; the list is
// capped at maxReferenceLocations.
// GET /api/lsp/references?path=internal/foo.go&line=42&char=13
func (s *Server) handleLSPReferences(w http.ResponseWriter, r *http.Request) {
	absPath, line0, char, ok := s.parseLSPParams(w, r)
	if !ok {
		return
	}
	mgr := s.lspManager()
	locations, err := mgr.References(absPath, line0, char)
	if err != nil {
		http.Error(w, fmt.Sprintf("lsp references: %v", err), http.StatusBadGateway)
		return
	}
	sess := s.session.Load()
	// Classification is memoized per file: references cluster in a handful
	// of files, and each classifyRoot call resolves symlinks.
	rc := newRootCache(s.lspRoot(), mgr.PeekRoots())
	sortReferences(locations, sess, rc)

	truncated := len(locations) > maxReferenceLocations
	if truncated {
		locations = locations[:maxReferenceLocations]
	}
	resp := make([]lspLocationResponse, 0, len(locations))
	for _, loc := range locations {
		resp = append(resp, resolveLocation(sess, loc, refPeekFullFileMaxLines, refPeekContextLines, rc))
	}
	writeJSON(w, map[string]any{"locations": resp, "truncated": truncated})
}

// referenceRank orders files by how likely the reviewer cares about them, so
// that the maxReferenceLocations cap drops the least relevant tail rather
// than everything alphabetically after the cut.
func referenceRank(path string, sess *Session, rc *rootCache) int {
	kind := rc.classify(path)
	if kind != rootRepo {
		return 2 // stdlib, module cache, elsewhere
	}
	if rel, ok := rc.relPath(path); ok && sess.FileByPath(rel) != nil {
		return 0 // a file under review
	}
	return 1 // elsewhere in the repo
}

// sortReferences orders locations by relevance rank, then path, line, and
// character. The character tiebreak matters: two references can share a line
// (x := x), and without it their order — and which one survives the cap —
// would be arbitrary.
func sortReferences(locations []lsp.Location, sess *Session, rc *rootCache) {
	rank := make(map[string]int, len(locations))
	for _, loc := range locations {
		if _, ok := rank[loc.Path]; !ok {
			rank[loc.Path] = referenceRank(loc.Path, sess, rc)
		}
	}
	sort.Slice(locations, func(i, j int) bool {
		a, b := locations[i], locations[j]
		if ra, rb := rank[a.Path], rank[b.Path]; ra != rb {
			return ra < rb
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Character < b.Character
	})
}

// resolveLocation classifies a target (session / repo / an extra root such
// as the stdlib or module cache) and attaches a peek when the file lives
// under a root crit is allowed to read. fullMaxLines/contextLines size the
// peek (definitions get generous windows, references small ones). Peek reads
// are restricted to paths the language server itself returned AND within the
// LSP root or an installed language's extra roots — there is deliberately no
// general file-read endpoint behind this.
func resolveLocation(sess *Session, loc lsp.Location, fullMaxLines, contextLines int, rc *rootCache) lspLocationResponse {
	out := lspLocationResponse{Line: loc.Line + 1}

	kind := rc.classify(loc.Path)
	if kind == rootRepo {
		relSlash, ok := rc.relPath(loc.Path)
		if !ok {
			relSlash = loc.Path
		}
		out.Path = relSlash
		out.DisplayPath = relSlash
		out.InRepo = true
		out.InSession = sess.FileByPath(relSlash) != nil
	} else {
		out.Path = loc.Path
		out.DisplayPath = displayPathOutsideRepo(loc.Path, kind, rc.extras)
	}
	if kind != rootNone {
		out.PeekStart, out.Peek, out.PeekTruncated = readPeek(loc.Path, loc.Line+1, fullMaxLines, contextLines)
	}
	out.LocalEnv = sess.Focus.Kind == FocusRange && kind >= 0 && kind < len(rc.extras) && rc.extras[kind].LocalEnv
	return out
}

// displayPathOutsideRepo shortens paths under an extra root (stdlib, module
// cache, global node_modules) to that root's display label for the UI.
func displayPathOutsideRepo(path string, kind int, extras []lsp.PeekRoot) string {
	if kind >= 0 && kind < len(extras) {
		if rel, err := filepath.Rel(extras[kind].Path, path); err == nil {
			return extras[kind].Label + "/" + filepath.ToSlash(rel)
		}
	}
	return path
}

// readPeek returns the file content around targetLine (1-based): the whole
// file when it is at most fullMaxLines long, otherwise a ±contextLines window
// (truncated=true).
func readPeek(absPath string, targetLine, fullMaxLines, contextLines int) (start int, lines []string, truncated bool) {
	f, err := os.Open(absPath)
	if err != nil {
		return 0, nil, false
	}
	defer f.Close()

	windowStart := targetLine - contextLines
	if windowStart < 1 {
		windowStart = 1
	}
	windowEnd := targetLine + contextLines

	// Scan line by line so a huge generated file costs the window, not the
	// whole file: once we know the file exceeds fullMaxLines AND the window
	// is fully collected, stop reading. Scanning (unlike splitting the raw
	// bytes on \n) also never yields a phantom empty line after a trailing
	// newline.
	var full, window []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := truncateLine(sc.Text())
		if n <= fullMaxLines {
			full = append(full, line)
		}
		if n >= windowStart && n <= windowEnd {
			window = append(window, line)
		}
		if n > fullMaxLines && n > windowEnd {
			break
		}
	}
	// A scan error — in practice a line longer than the buffer, which
	// generated code can hit — stops the loop early, so what we collected is
	// a prefix of the file and must never be advertised as a whole-file peek.
	if n <= fullMaxLines && sc.Err() == nil {
		if len(full) == 0 || windowStart > n {
			return 0, nil, false
		}
		return 1, full, false
	}
	if len(window) == 0 {
		return 0, nil, false
	}
	return windowStart, window, true
}

// truncateLine caps pathological lines (minified/generated code) at
// peekMaxLineLen bytes, backing up to a rune boundary so a multi-byte
// character is never split (a mid-rune cut renders as U+FFFD in the peek).
func truncateLine(line string) string {
	if len(line) <= peekMaxLineLen {
		return line
	}
	end := peekMaxLineLen
	for end > 0 && !utf8.RuneStart(line[end]) {
		end--
	}
	return line[:end] + "…"
}

// pathWithinRoot adapts pathsafe.ResolveUnder for callers that only need the
// yes/no answer.
func pathWithinRoot(path, root string) bool {
	_, err := pathsafe.ResolveUnder(path, root)
	return err == nil
}
