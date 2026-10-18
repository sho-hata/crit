package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newPreviewContentTestServer(t *testing.T, htmlFile string) *Server {
	t.Helper()
	sess := &Session{
		Mode:        "files",
		RepoRoot:    filepath.Dir(htmlFile),
		ReviewRound: 1,
		ReviewType:  "preview",
		Origin:      htmlFile,
		Files: []*FileEntry{
			{
				Path:     filepath.Base(htmlFile),
				Status:   "added",
				FileType: "code",
				Content:  "<html><body>test</body></html>",
			},
		},
	}
	sess.InitTestChannels()
	s, err := NewServer(sess, frontendFS, "", "test", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServePreviewHTML_AppendsAgentWhenNoBodyTag(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	htmlFile := filepath.Join(dir, "index.html")
	if err := os.WriteFile(htmlFile, []byte("<div>fragment with no body tag</div>"), 0644); err != nil {
		t.Fatal(err)
	}
	s := newPreviewContentTestServer(t, htmlFile)

	req := httptest.NewRequest("GET", "/preview-content/", nil)
	w := httptest.NewRecorder()
	s.handlePreviewContent(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "fragment with no body tag") {
		t.Error("response missing original content")
	}
	if !strings.Contains(body, "crit-agent.js") {
		t.Error("agent script not appended when </body> is absent")
	}
	fragIdx := strings.Index(body, "fragment with no body tag")
	agentIdx := strings.Index(body, "crit-agent.js")
	if fragIdx < 0 || agentIdx < 0 || agentIdx < fragIdx {
		t.Error("expected agent scripts appended after fragment content")
	}
}

// TestPreviewContent_SandboxCSP pins the isolation of untrusted preview
// content: every response from /preview-content (the previewed page, a
// sibling page, an SVG, an XHTML file) carries a CSP sandbox without
// allow-same-origin, so script in the preview runs in an opaque origin and
// cannot drive the Crit API as the UI.
func TestPreviewContent_SandboxCSP(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	htmlFile := filepath.Join(dir, "index.html")
	files := map[string]string{
		"index.html": "<html><body>root</body></html>",
		"other.html": "<html><body>other</body></html>",
		"logo.svg":   `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"page.xhtml": `<html xmlns="http://www.w3.org/1999/xhtml"><body/></html>`,
		"style.css":  "body{}",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	s := newPreviewContentTestServer(t, htmlFile)

	paths := []string{
		"/preview-content",
		"/preview-content/",
		"/preview-content/other.html",
		"/preview-content/logo.svg",
		"/preview-content/page.xhtml",
		"/preview-content/style.css",
	}
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, w.Code)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.HasPrefix(csp, "sandbox ") || !strings.Contains(csp, "allow-scripts") {
			t.Errorf("%s: CSP = %q, want a sandbox that allows scripts", path, csp)
		}
		if strings.Contains(csp, "allow-same-origin") {
			t.Errorf("%s: CSP = %q must not grant allow-same-origin", path, csp)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
	}
}

// TestPreviewContent_OpaqueOriginSubresourceCORS checks that the sandboxed
// preview can still load module scripts and fonts (CORS-mode requests sent
// with Origin: null), while fetch()/XHR reads get no ACAO header.
func TestPreviewContent_OpaqueOriginSubresourceCORS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	htmlFile := filepath.Join(dir, "index.html")
	for name, body := range map[string]string{
		"index.html": "<html><body></body></html>",
		"app.js":     "export const x = 1;",
		"font.woff2": "font",
		"data.json":  `{"secret":true}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	s := newPreviewContentTestServer(t, htmlFile)

	tests := []struct {
		name, path, origin, dest, wantACAO string
	}{
		{"module script from sandbox", "/preview-content/app.js", "null", "script", "*"},
		{"font from sandbox", "/preview-content/font.woff2", "null", "font", "*"},
		{"fetch from sandbox", "/preview-content/data.json", "null", "empty", ""},
		{"module script from another site", "/preview-content/app.js", "https://evil.example", "script", ""},
		{"no origin", "/preview-content/app.js", "", "script", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			req.Header.Set("Sec-Fetch-Dest", tt.dest)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != tt.wantACAO {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantACAO)
			}
			if got := w.Header().Get("Vary"); got != "Origin, Sec-Fetch-Dest" {
				t.Errorf("Vary = %q, want %q", got, "Origin, Sec-Fetch-Dest")
			}
		})
	}
}
