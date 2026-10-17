package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
