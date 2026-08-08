package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/tomasz-tomczyk/crit/internal/lsp"
	"github.com/tomasz-tomczyk/crit/internal/pathsafe"
)

// lspProvider is the slice of lsp.Manager the handlers need; an interface so
// tests can inject a fake without spawning gopls.
type lspProvider interface {
	Hover(absPath string, line, character int) (string, error)
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
// gopls itself is spawned even later — on the first Hover inside the manager
// (lazy start keeps parallel worktree daemons cheap).
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

	cleaned := filepath.ToSlash(filepath.Clean(reqPath))
	if filepath.IsAbs(reqPath) || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
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

// pathWithinRoot adapts pathsafe.ResolveUnder for callers that only need the
// yes/no answer.
func pathWithinRoot(path, root string) bool {
	_, err := pathsafe.ResolveUnder(path, root)
	return err == nil
}
