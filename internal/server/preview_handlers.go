package server

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sho-hata/crit/internal/pathsafe"
)

// previewContentCSP isolates the previewed document from the Crit origin.
// The sandbox directive without allow-same-origin gives the document an
// opaque origin, so its requests to /api/* carry `Origin: null` and
// `Sec-Fetch-Site: cross-site` and the CSRF gate rejects every state-changing
// call (trusting project hooks, finishing the review, invoking agent_cmd).
// Scripts still run so the preview renders and the injected agent can talk
// to the chrome over postMessage. Never add allow-same-origin here: a
// same-origin sandboxed document can lift its own sandbox.
const previewContentCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads"

// repoFileCSP sandboxes documents served from /files/ (an .html, .svg or
// .xml file in the reviewed tree opened directly) with no script at all.
// The Crit UI only embeds those files as images, where CSP does not apply.
const repoFileCSP = "sandbox"

// setUntrustedContentHeaders marks a response whose bytes come from the
// tree under review: a document rendered from it must never share the
// Crit API's origin.
func setUntrustedContentHeaders(w http.ResponseWriter, csp string) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// allowOpaqueOriginSubresource lets the sandboxed preview load the CORS-mode
// sub-resources a page needs to render (module scripts, web fonts). They are
// requested from the preview's opaque origin (`Origin: null`) and would fail
// without an ACAO header. fetch()/XHR (Sec-Fetch-Dest: empty) stay excluded:
// granting them would let any opaque-origin page read files next to the
// previewed one. A script or font load exposes nothing a classic <script> or
// <link> could not already pull in. Vary is sent on every response, granted
// or not: otherwise a page could prime the HTTP cache with a font-mode load
// and replay the ACAO response to fetch(..., {cache: 'force-cache'}).
// Script grants are limited to JavaScript files: JSON modules
// (import(..., {with: {type: 'json'}})) also load with the script destination
// and would hand the page the parsed file.
func allowOpaqueOriginSubresource(w http.ResponseWriter, r *http.Request, path string) {
	w.Header().Add("Vary", "Origin, Sec-Fetch-Dest")
	if r.Header.Get("Origin") != "null" {
		return
	}
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "script":
		if isJavaScriptPath(path) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
	case "font":
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
}

func isJavaScriptPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs", ".cjs":
		return true
	}
	return false
}

// handlePreviewContent serves the previewed HTML file and its sibling assets.
func (s *Server) handlePreviewContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	setUntrustedContentHeaders(w, previewContentCSP)
	sess := s.session.Load()
	if sess == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	if sess.Origin == "" {
		http.Error(w, "no preview file configured", http.StatusNotFound)
		return
	}

	reqPath := strings.TrimPrefix(r.URL.Path, "/preview-content")
	baseDir := filepath.Dir(sess.Origin)

	if reqPath == "" || reqPath == "/" {
		s.servePreviewHTML(w, sess.Origin)
		return
	}

	// Resolve symlinks before the containment check: a lexical prefix check
	// would let a symlink in the preview directory serve files outside it.
	resolved, err := pathsafe.ResolveUnder(filepath.Join(baseDir, filepath.Clean(reqPath)), baseDir)
	if err != nil {
		if errors.Is(err, pathsafe.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "forbidden", http.StatusForbidden)
		}
		return
	}

	info, err := os.Stat(resolved)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if info.IsDir() {
		http.NotFound(w, r)
		return
	}

	if isPreviewHTMLPath(resolved) {
		s.servePreviewHTML(w, resolved)
		return
	}

	allowOpaqueOriginSubresource(w, r, resolved)
	http.ServeFile(w, r, resolved)
}

func isPreviewHTMLPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".html" || ext == ".htm"
}

func (s *Server) servePreviewHTML(w http.ResponseWriter, filePath string) {
	body, err := os.ReadFile(filePath)
	if err != nil {
		http.Error(w, "failed to read preview file", http.StatusInternalServerError)
		return
	}

	var sb strings.Builder
	for _, f := range AgentScriptFiles {
		sb.WriteString(`<script src="/`)
		sb.WriteString(f)
		sb.WriteString(`"></script>`)
	}
	agentScripts := sb.String()

	// Insert before the last </body>, or append when the file is a fragment
	// with no closing body tag (matches crit-web raw_controller).
	idx := bytes.LastIndex(bytes.ToLower(body), []byte("</body>"))
	if idx >= 0 {
		var out []byte
		out = append(out, body[:idx]...)
		out = append(out, []byte(agentScripts)...)
		out = append(out, body[idx:]...)
		body = out
	} else {
		body = append(body, []byte(agentScripts)...)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(body)
}
