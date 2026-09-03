package server

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
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

// goLspSparsePatterns is the sparse-checkout pattern set for the range-focus
// LSP worktree: Go source and module files, enough for gopls without the
// rest of the tree.
var goLspSparsePatterns = []string{"*.go", "go.mod", "go.sum", "go.work", "go.work.sum"}

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
	GoEnv() (goroot, gomodcache string)
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
	// idleTimer drops worktreeDir (and prov) after lsp.DefaultIdleTimeout
	// without an LSP request, matching gopls's own idle shutdown so a
	// quietly-abandoned range-focus review doesn't keep a checkout on disk.
	idleTimer *time.Timer
	// idleTimeout overrides lsp.DefaultIdleTimeout in tests.
	idleTimeout time.Duration
	// newProvider creates the provider on first use; tests override it.
	newProvider func() lspProvider
	// binaryAvailable overrides the gopls PATH lookup in tests.
	binaryAvailable func() bool
}

// lspAvailable reports whether LSP features should be offered to the
// frontend: enabled in config, gopls installed, and a repo root to anchor the
// workspace.
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
	if s.lsp.binaryAvailable != nil {
		return s.lsp.binaryAvailable()
	}
	return lsp.GoplsAvailable()
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
	if s.lsp.worktreeDir != "" && s.lsp.worktreeSHA == sess.Focus.HeadSHA {
		s.touchLSPIdleLocked()
		return nil // already rooted at this commit
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
		size, err := vcs.SparseTreeSize(s.shutdownCtx, sess.RepoRoot, sess.Focus.HeadSHA, goLspSparsePatterns)
		if err != nil {
			return fmt.Errorf("estimating lsp worktree size: %w", err)
		}
		if limitBytes := int64(limitMB) * 1024 * 1024; size > limitBytes {
			return fmt.Errorf("lsp worktree would be %dMB, over the %dMB lsp_worktree_max_mb limit", size/(1024*1024), limitMB)
		}
	}

	if err := vcs.AddSparseWorktree(s.shutdownCtx, sess.RepoRoot, sess.Focus.HeadSHA, dir, goLspSparsePatterns); err != nil {
		return fmt.Errorf("preparing lsp worktree: %w", err)
	}
	s.lsp.worktreeDir = dir
	s.lsp.worktreeSHA = sess.Focus.HeadSHA
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
			s.lsp.prov = lsp.NewManager(s.lspRootLocked(), s.shutdownCtx)
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
	if reqPath == "" || !strings.HasSuffix(reqPath, ".go") {
		http.Error(w, "path must be a .go file", http.StatusBadRequest)
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
	root := s.lspRoot()

	if filepath.IsAbs(reqPath) {
		// Absolute paths support chained jumps from the peek popup. They are
		// accepted ONLY under the same roots the peek itself may read (repo
		// root / GOROOT / GOMODCACHE) — this endpoint must not become a
		// general filesystem probe.
		absPath = filepath.Clean(reqPath)
		if !s.lspPathAllowed(absPath, root) {
			http.Error(w, "Access denied", http.StatusForbidden)
			return "", 0, 0, false
		}
		return absPath, line - 1, char, true
	}

	cleaned := filepath.ToSlash(filepath.Clean(reqPath))
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		http.Error(w, "Invalid file path", http.StatusBadRequest)
		return "", 0, 0, false
	}
	absPath = filepath.Join(root, filepath.FromSlash(cleaned))
	if !pathWithinRoot(absPath, root) {
		http.Error(w, "Access denied", http.StatusForbidden)
		return "", 0, 0, false
	}
	return absPath, line - 1, char, true
}

// rootKind classifies which allowed root contains an LSP path.
type rootKind int

const (
	rootNone rootKind = iota
	rootRepo
	rootGoroot
	rootGomodcache
)

// rootCache memoizes classifyRoot per path for one request. References
// return many locations concentrated in few files, and each classification
// resolves symlinks against up to three roots.
type rootCache struct {
	root, goroot, gomodcache string
	seen                     map[string]rootKind
}

func newRootCache(root, goroot, gomodcache string) *rootCache {
	return &rootCache{
		root:       root,
		goroot:     goroot,
		gomodcache: gomodcache,
		seen:       make(map[string]rootKind),
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

func (c *rootCache) classify(absPath string) rootKind {
	if kind, ok := c.seen[absPath]; ok {
		return kind
	}
	kind := classifyRoot(absPath, c.root, c.goroot, c.gomodcache)
	c.seen[absPath] = kind
	return kind
}

// classifyRoot resolves absPath against the three roots LSP features may
// touch (LSP root, GOROOT, GOMODCACHE) and reports which one contains it. This is the single source of
// truth for authorization (lspPathAllowed), peek readability, and display
// formatting — the classification must never drift between those uses.
func classifyRoot(absPath, root, goroot, gomodcache string) rootKind {
	if pathWithinRoot(absPath, root) {
		return rootRepo
	}
	if goroot != "" && pathWithinRoot(absPath, goroot) {
		return rootGoroot
	}
	if gomodcache != "" && pathWithinRoot(absPath, gomodcache) {
		return rootGomodcache
	}
	return rootNone
}

// lspPathAllowed reports whether an absolute path lies under one of the
// roots LSP features may touch: the LSP root, GOROOT, or GOMODCACHE.
func (s *Server) lspPathAllowed(absPath, root string) bool {
	goroot, gomodcache := s.lspManager().GoEnv()
	return classifyRoot(absPath, root, goroot, gomodcache) != rootNone
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
	writeJSON(w, map[string]any{"contents": contents})
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
	goroot, gomodcache := mgr.GoEnv()
	rc := newRootCache(s.lspRoot(), goroot, gomodcache)
	resp := make([]lspLocationResponse, 0, len(locations))
	for _, loc := range locations {
		resp = append(resp, resolveLocation(sess, loc, goroot, gomodcache, peekFullFileMaxLines, peekContextLines, rc))
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
	goroot, gomodcache := mgr.GoEnv()
	// Classification is memoized per file: references cluster in a handful
	// of files, and each classifyRoot call resolves symlinks.
	rc := newRootCache(s.lspRoot(), goroot, gomodcache)
	sortReferences(locations, sess, rc)

	truncated := len(locations) > maxReferenceLocations
	if truncated {
		locations = locations[:maxReferenceLocations]
	}
	resp := make([]lspLocationResponse, 0, len(locations))
	for _, loc := range locations {
		resp = append(resp, resolveLocation(sess, loc, goroot, gomodcache, refPeekFullFileMaxLines, refPeekContextLines, rc))
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

// resolveLocation classifies a target (session / repo / stdlib / module
// cache) and attaches a peek when the file lives under a root crit is allowed
// to read. fullMaxLines/contextLines size the peek (definitions get generous
// windows, references small ones). Peek reads are restricted to paths gopls
// itself returned AND within repoRoot, GOROOT, or GOMODCACHE — there is
// deliberately no general file-read endpoint behind this.
func resolveLocation(sess *Session, loc lsp.Location, goroot, gomodcache string, fullMaxLines, contextLines int, rc *rootCache) lspLocationResponse {
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
		out.DisplayPath = displayPathOutsideRepo(loc.Path, kind, goroot, gomodcache)
	}
	if kind != rootNone {
		out.PeekStart, out.Peek, out.PeekTruncated = readPeek(loc.Path, loc.Line+1, fullMaxLines, contextLines)
	}
	return out
}

// displayPathOutsideRepo shortens stdlib and module-cache paths for the UI.
func displayPathOutsideRepo(path string, kind rootKind, goroot, gomodcache string) string {
	switch kind {
	case rootGoroot:
		if rel, err := filepath.Rel(goroot, path); err == nil {
			return "$GOROOT/" + filepath.ToSlash(rel)
		}
	case rootGomodcache:
		if rel, err := filepath.Rel(gomodcache, path); err == nil {
			return "$GOMODCACHE/" + filepath.ToSlash(rel)
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
