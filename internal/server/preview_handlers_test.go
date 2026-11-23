package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
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
		"lib.MJS":    "export const y = 2;",
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
		{"JSON module from sandbox", "/preview-content/data.json", "null", "script", ""},
		{"mjs module from sandbox", "/preview-content/lib.MJS", "null", "script", "*"},
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

// TestHandlePreviewContent_PathContainment verifies that sibling assets are
// served only when their symlink-resolved target stays inside the preview
// file's directory.
func TestHandlePreviewContent_PathContainment(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on Windows")
	}

	root := t.TempDir()
	siteDir := filepath.Join(root, "site")
	outsideDir := filepath.Join(root, "outside")
	for _, d := range []string{siteDir, outsideDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(siteDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	htmlFile := filepath.Join(siteDir, "index.html")
	write(htmlFile, "<html><body>page</body></html>")
	write(filepath.Join(siteDir, "app.js"), "inside-asset")
	write(filepath.Join(root, "parent-secret.txt"), "parent-secret")
	write(filepath.Join(outsideDir, "secret.txt"), "outside-secret")
	link(filepath.Join(outsideDir, "secret.txt"), "leak.txt")
	link(outsideDir, "leakdir")
	link("app.js", "alias.js")

	s := newPreviewContentTestServer(t, htmlFile)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"normal asset", "/preview-content/app.js", http.StatusOK, "inside-asset"},
		{"in-directory symlink", "/preview-content/alias.js", http.StatusOK, "inside-asset"},
		{"symlinked file escape", "/preview-content/leak.txt", http.StatusForbidden, ""},
		{"symlinked dir escape", "/preview-content/leakdir/secret.txt", http.StatusForbidden, ""},
		{"dot-dot traversal", "/preview-content/../parent-secret.txt", http.StatusNotFound, ""},
		{"missing file", "/preview-content/nope.js", http.StatusNotFound, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("GET", tc.path, nil)
			w := httptest.NewRecorder()
			s.handlePreviewContent(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.wantStatus, w.Body.String())
			}
			body := w.Body.String()
			if tc.wantBody != "" && !strings.Contains(body, tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", body, tc.wantBody)
			}
			if strings.Contains(body, "secret") {
				t.Errorf("response leaked a file outside the preview directory: %q", body)
			}
		})
	}
}

func TestServePreviewExcalidraw_ServesCanvasPageWithSceneAndAgent(t *testing.T) {
	dir := t.TempDir()
	scene := filepath.Join(dir, "arch & flow.excalidraw")
	// A text element whose words would close the script element if inlined raw.
	content := `{"type":"excalidraw","elements":[{"id":"t","type":"text","text":"</script><script>alert(1)</script>"}]}`
	if err := os.WriteFile(scene, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	s := newPreviewContentTestServer(t, scene)

	req := httptest.NewRequest("GET", "/preview-content/", nil)
	w := httptest.NewRecorder()
	s.handlePreviewContent(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<title>arch &amp; flow.excalidraw</title>") {
		t.Error("title is not the escaped file name")
	}
	if strings.Contains(body, "{{SCENE}}") || strings.Contains(body, "{{TITLE}}") {
		t.Error("template placeholders left in the page")
	}
	if strings.Contains(body, "</script><script>alert(1)") {
		t.Error("scene text was inlined unescaped")
	}
	if !strings.Contains(body, `\u003c/script\u003e\u003cscript\u003ealert(1)`) {
		t.Error("scene not inlined as an escaped JSON string")
	}
	for _, script := range []string{"/crit-renderer-excalidraw.js", "/crit-excalidraw-canvas.js", "/rough.min.js", "crit-agent.js"} {
		if !strings.Contains(body, script) {
			t.Errorf("page does not load %s", script)
		}
	}
	if strings.Index(body, "crit-excalidraw-canvas.js") > strings.Index(body, "crit-agent.js") {
		t.Error("the canvas must be drawn before the agent scripts run")
	}
}

func TestServePreviewExcalidraw_SceneGoneIsAServerError(t *testing.T) {
	dir := t.TempDir()
	scene := filepath.Join(dir, "arch.excalidraw")
	if err := os.WriteFile(scene, []byte(`{"elements":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	s := newPreviewContentTestServer(t, scene)
	if err := os.Remove(scene); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handlePreviewContent(w, httptest.NewRequest("GET", "/preview-content/", nil))

	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "failed to read preview file") {
		t.Fatalf("got %d %q, want 500 failed to read preview file", w.Code, w.Body.String())
	}
}

func TestServePreviewExcalidraw_MissingCanvasPageIsAServerError(t *testing.T) {
	dir := t.TempDir()
	scene := filepath.Join(dir, "arch.excalidraw")
	if err := os.WriteFile(scene, []byte(`{"elements":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	s := newPreviewContentTestServer(t, scene)
	s.assets = fstest.MapFS{}

	w := httptest.NewRecorder()
	s.handlePreviewContent(w, httptest.NewRequest("GET", "/preview-content/", nil))

	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "missing excalidraw-canvas.html") {
		t.Fatalf("got %d %q, want 500 missing excalidraw-canvas.html", w.Code, w.Body.String())
	}
}

func TestServePreviewExcalidraw_SiblingFilesServedAsIs(t *testing.T) {
	dir := t.TempDir()
	scene := filepath.Join(dir, "arch.excalidraw")
	if err := os.WriteFile(scene, []byte(`{"elements":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	s := newPreviewContentTestServer(t, scene)

	req := httptest.NewRequest("GET", "/preview-content/arch.excalidraw", nil)
	w := httptest.NewRecorder()
	s.handlePreviewContent(w, req)

	if w.Code != http.StatusOK || w.Body.String() != `{"elements":[]}` {
		t.Fatalf("got %d %q, want the raw scene", w.Code, w.Body.String())
	}
}
