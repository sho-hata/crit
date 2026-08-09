package server

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tomasz-tomczyk/crit/internal/lsp"
	"github.com/tomasz-tomczyk/crit/internal/pathsafe"
)

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

// lspProvider is the slice of lsp.Manager the handlers need; an interface so
// tests can inject a fake without spawning gopls.
type lspProvider interface {
	Hover(absPath string, line, character int) (string, error)
	Definition(absPath string, line, character int) ([]lsp.Location, error)
	GoEnv() (goroot, gomodcache string)
	Shutdown()
}

// lspState holds the lazily-created LSP manager. Lives on Server via
// composition (see server.go).
type lspState struct {
	mu   sync.Mutex
	prov lspProvider
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
	// Range/PR focus renders file content at Focus.HeadSHA, but the LSP
	// endpoints read the working tree — positions could silently resolve
	// against different content than the reviewer sees. Disable rather than
	// answer wrong. (Feeding SHA content to gopls as a didOpen overlay is a
	// possible future improvement.)
	if sess.Focus.Kind == FocusRange {
		return false
	}
	if s.lsp.binaryAvailable != nil {
		return s.lsp.binaryAvailable()
	}
	return lsp.GoplsAvailable()
}

// lspManager returns the shared LSP provider, creating it on first call.
// gopls itself is spawned even later — on the first Hover/Definition inside
// the manager (lazy start keeps parallel worktree daemons cheap).
func (s *Server) lspManager() lspProvider {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	if s.lsp.prov == nil {
		if s.lsp.newProvider != nil {
			s.lsp.prov = s.lsp.newProvider()
		} else {
			// shutdownCtx bounds the gopls subprocess: SIGINT/SIGTERM on the
			// daemon kills it instead of leaking.
			s.lsp.prov = lsp.NewManager(s.session.Load().RepoRoot, s.shutdownCtx)
		}
	}
	return s.lsp.prov
}

// ShutdownLSP stops the language server if one was started.
func (s *Server) ShutdownLSP() {
	s.lsp.mu.Lock()
	defer s.lsp.mu.Unlock()
	if s.lsp.prov != nil {
		s.lsp.prov.Shutdown()
		s.lsp.prov = nil
	}
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
	repoRoot := s.session.Load().RepoRoot

	if filepath.IsAbs(reqPath) {
		// Absolute paths support chained jumps from the peek popup. They are
		// accepted ONLY under the same roots the peek itself may read (repo
		// root / GOROOT / GOMODCACHE) — this endpoint must not become a
		// general filesystem probe.
		absPath = filepath.Clean(reqPath)
		if !s.lspPathAllowed(absPath, repoRoot) {
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
	absPath = filepath.Join(repoRoot, filepath.FromSlash(cleaned))
	if !pathWithinRoot(absPath, repoRoot) {
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

// classifyRoot resolves absPath against the three roots LSP features may
// touch and reports which one contains it. This is the single source of
// truth for authorization (lspPathAllowed), peek readability, and display
// formatting — the classification must never drift between those uses.
func classifyRoot(absPath, repoRoot, goroot, gomodcache string) rootKind {
	if pathWithinRoot(absPath, repoRoot) {
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
// roots LSP features may touch: repo root, GOROOT, or GOMODCACHE.
func (s *Server) lspPathAllowed(absPath, repoRoot string) bool {
	goroot, gomodcache := s.lspManager().GoEnv()
	return classifyRoot(absPath, repoRoot, goroot, gomodcache) != rootNone
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
	resp := make([]lspLocationResponse, 0, len(locations))
	for _, loc := range locations {
		resp = append(resp, s.resolveLocation(sess, loc, goroot, gomodcache))
	}
	writeJSON(w, map[string]any{"locations": resp})
}

// resolveLocation classifies a definition target (session / repo / stdlib /
// module cache) and attaches a peek when the file lives under a root crit is
// allowed to read. Peek reads are restricted to paths gopls itself returned
// AND within repoRoot, GOROOT, or GOMODCACHE — there is deliberately no
// general file-read endpoint behind this.
func (s *Server) resolveLocation(sess *Session, loc lsp.Location, goroot, gomodcache string) lspLocationResponse {
	repoRoot := sess.RepoRoot
	out := lspLocationResponse{Line: loc.Line + 1}

	kind := classifyRoot(loc.Path, repoRoot, goroot, gomodcache)
	if kind == rootRepo {
		rel, err := filepath.Rel(repoRoot, loc.Path)
		if err != nil {
			rel = loc.Path
		}
		relSlash := filepath.ToSlash(rel)
		out.Path = relSlash
		out.DisplayPath = relSlash
		out.InRepo = true
		out.InSession = sess.FileByPath(relSlash) != nil
	} else {
		out.Path = loc.Path
		out.DisplayPath = displayPathOutsideRepo(loc.Path, kind, goroot, gomodcache)
	}
	if kind != rootNone {
		out.PeekStart, out.Peek, out.PeekTruncated = readPeek(loc.Path, loc.Line+1)
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
// file when it is small enough, otherwise a ±peekContextLines window
// (truncated=true).
func readPeek(absPath string, targetLine int) (start int, lines []string, truncated bool) {
	f, err := os.Open(absPath)
	if err != nil {
		return 0, nil, false
	}
	defer f.Close()

	windowStart := targetLine - peekContextLines
	if windowStart < 1 {
		windowStart = 1
	}
	windowEnd := targetLine + peekContextLines

	// Scan line by line so a huge generated file costs the window, not the
	// whole file: once we know the file exceeds peekFullFileMaxLines AND the
	// window is fully collected, stop reading. Scanning (unlike splitting the
	// raw bytes on \n) also never yields a phantom empty line after a
	// trailing newline.
	var full, window []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := truncateLine(sc.Text())
		if n <= peekFullFileMaxLines {
			full = append(full, line)
		}
		if n >= windowStart && n <= windowEnd {
			window = append(window, line)
		}
		if n > peekFullFileMaxLines && n > windowEnd {
			break
		}
	}
	if n <= peekFullFileMaxLines {
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
