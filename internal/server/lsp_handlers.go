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

// lspSparsePatterns limits the range-focus worktree to languages that both
// appear in the review and have a server installed, so absent languages don't
// inflate the checkout or its lsp_worktree_max_mb estimate.
func (s *Server) lspSparsePatterns(sess *Session) []string {
	paths := make([]string, 0, len(sess.Files))
	for _, f := range sess.Files {
		paths = append(paths, f.Path)
	}
	return lsp.SparsePatternsForFiles(paths, s.lspLangAvailable())
}

// Above peekFullFileMaxLines (generated code in the module cache can reach
// tens of thousands of lines) the peek is a ±peekContextLines window so
// neither the JSON payload nor the frontend's per-line highlighting balloons.
const (
	peekFullFileMaxLines = 2000
	peekContextLines     = 100
	peekMaxLineLen       = 500
)

// References can return many locations, so each carries a much smaller peek
// window than a definition (the list row shows one line; the window only
// feeds the click-to-peek popup) and the total count is capped.
const (
	refPeekFullFileMaxLines = 50
	refPeekContextLines     = 10
	maxReferenceLocations   = 200
)

// lspProvider lets tests inject a fake instead of spawning language servers.
type lspProvider interface {
	Hover(absPath string, line, character int) (string, error)
	Definition(absPath string, line, character int) ([]lsp.Location, error)
	References(absPath string, line, character int) ([]lsp.Location, error)
	PeekRoots() []lsp.PeekRoot
	Shutdown()
}

type lspState struct {
	mu   sync.Mutex
	prov lspProvider
	// worktreeDir is the range/PR focus sparse checkout prov is rooted at;
	// empty when rooted at the working tree.
	worktreeDir string
	worktreeSHA string
	// Patterns depend on which servers are installed, which can change while
	// a daemon runs, so SHA equality alone is not enough to reuse a checkout.
	worktreePatterns []string
	// idleTimer drops the worktree (and prov) on the same idle timeout as the
	// servers, so an abandoned range-focus review doesn't keep a checkout on
	// disk.
	idleTimer *time.Timer

	// Test hooks.
	idleTimeout   time.Duration
	newProvider   func() lspProvider
	langAvailable func(*lsp.Language) bool
}

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

func (s *Server) lspLangAvailable() func(*lsp.Language) bool {
	if s.lsp.langAvailable != nil {
		return s.lsp.langAvailable
	}
	return (*lsp.Language).Available
}

// lspExtensions is sent via /api/config so the frontend only offers LSP on
// files a server can actually answer for.
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

// lspRoot is the server workspace, the base for request and result paths,
// and the root peek reads are authorized against. Every LSP code path must
// use it rather than sess.RepoRoot, so pointing the workspace at a range/PR
// focus checkout moves all of these together.
func (s *Server) lspRoot() string {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	return s.lspRootLocked()
}

// lspRootLocked is lspRoot for callers holding s.lsp.mu. sync.Mutex is not
// reentrant: a locked caller (lspManager) routing through lspRoot
// self-deadlocks and leaves the mutex held for the daemon's lifetime.
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
// Language servers read the disk, but in range/PR focus the pane shows files
// at Focus.HeadSHA, which can differ from the working tree — so HeadSHA is
// checked out into a sparse worktree and LSP rooted there. Must run before
// lspRoot/lspManager on every request.
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
		return nil
	}

	// Rebuilt here rather than on focus change: a review may switch PRs
	// several times before anyone hovers a symbol.
	s.dropLSPRootLocked(sess)

	dir := session.ReviewPathsFor(s.reviewPath).LSPWorktree
	if _, err := os.Lstat(dir); err == nil {
		// Left behind by a daemon that crashed before cleaning up;
		// AddSparseWorktree refuses an existing dir.
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

// Caller holds s.lsp.mu.
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

// dropIdleLSPRoot runs on the timer's goroutine, so it takes s.lsp.mu itself.
func (s *Server) dropIdleLSPRoot() {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	s.dropLSPRootLocked(s.session.Load())
}

// dropLSPRootLocked shuts down the provider and removes its worktree, if
// any. A failed removal is only logged: on the syncLSPRoot path the leftover
// fails the following AddSparseWorktree anyway. Caller holds s.lsp.mu.
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

// lspManager creates the provider on first call; the servers themselves are
// spawned later still, on their first request (keeps parallel worktree
// daemons cheap). Request-path callers must call syncLSPRoot first.
func (s *Server) lspManager() lspProvider {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	if s.lsp.prov == nil {
		if s.lsp.newProvider != nil {
			s.lsp.prov = s.lsp.newProvider()
		} else {
			// The working tree is passed separately: under range/PR focus,
			// git-untracked dependencies live only there (Manager.depRoot).
			var repoRoot string
			if sess := s.session.Load(); sess != nil {
				repoRoot = sess.RepoRoot
			}
			s.lsp.prov = lsp.NewManager(s.lspRootLocked(), repoRoot, s.shutdownCtx)
		}
	}
	return s.lsp.prov
}

// ShutdownLSP is called on daemon shutdown.
func (s *Server) ShutdownLSP() {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	s.dropLSPRootLocked(s.session.Load())
}

// parseLSPParams: line is 1-based (the UI's NewNum); char is a 0-based
// UTF-16 offset passed through verbatim — LSP's default encoding and JS
// strings are both UTF-16.
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
	// Uninstalled is a client error; letting it reach startServer would
	// surface as a misleading 502.
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

func (s *Server) resolveLSPRequestPath(w http.ResponseWriter, reqPath string) (string, bool) {
	root := s.lspRoot()

	if filepath.IsAbs(reqPath) {
		// Absolute paths (chained jumps from the peek popup) are accepted ONLY
		// under the roots the peek itself may read — this endpoint must not
		// become a general filesystem probe.
		absPath := filepath.Clean(reqPath)
		if classifyRoot(absPath, root, s.lspManager().PeekRoots()) == rootNone {
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

// rootCache memoizes classifyRoot per request: references cluster in few
// files, and each classification resolves symlinks against every root.
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

// relPath returns the slash-separated form the frontend and
// Session.FileByPath use.
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

// classifyRoot is the single source of truth for request-path authorization,
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

// lspLocalEnv: under range/PR focus, a LocalEnv language resolves
// third-party packages against the reviewer's environment, not the reviewed
// SHA's. In working-tree focus they are the same tree.
func (s *Server) lspLocalEnv(absPath string) bool {
	sess := s.session.Load()
	if sess == nil || sess.Focus.Kind != FocusRange {
		return false
	}
	lang := lsp.LanguageForPath(absPath)
	return lang != nil && lang.LocalEnv
}

type lspLocationResponse struct {
	// Path is repo-relative (slash-separated) when InRepo, absolute otherwise.
	Path        string   `json:"path"`
	DisplayPath string   `json:"display_path"`
	Line        int      `json:"line"`      // 1-based
	Character   int      `json:"character"` // 0-based UTF-16
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

// Each location carries a peek so the frontend can render it even when the
// target is outside the visible diff.
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

// sortReferences: the character tiebreak matters — two references can share
// a line (x := x), and without it which one survives the cap is arbitrary.
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

// resolveLocation: peek reads are restricted to paths the language server
// itself returned AND within the LSP root or an installed language's extra
// roots — there is deliberately no general file-read endpoint behind this.
func resolveLocation(sess *Session, loc lsp.Location, fullMaxLines, contextLines int, rc *rootCache) lspLocationResponse {
	out := lspLocationResponse{Line: loc.Line + 1, Character: loc.Character}

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

func displayPathOutsideRepo(path string, kind int, extras []lsp.PeekRoot) string {
	if kind >= 0 && kind < len(extras) {
		if rel, err := filepath.Rel(extras[kind].Path, path); err == nil {
			return extras[kind].Label + "/" + filepath.ToSlash(rel)
		}
	}
	return path
}

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

	// Scanning stops once the file is known to exceed fullMaxLines and the
	// window is collected, so a huge generated file costs only the window.
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
	// A scan error (a line longer than the buffer, in generated code) leaves
	// only a prefix, which must never be advertised as a whole-file peek.
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

// truncateLine backs up to a rune boundary: a mid-rune cut renders as U+FFFD.
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

func pathWithinRoot(path, root string) bool {
	_, err := pathsafe.ResolveUnder(path, root)
	return err == nil
}
