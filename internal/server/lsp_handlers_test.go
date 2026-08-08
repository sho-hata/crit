package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tomasz-tomczyk/crit/internal/config"
)

// fakeLSPProvider implements lspProvider without spawning gopls.
type fakeLSPProvider struct {
	hoverContents string
	hoverErr      error
	shutdowns     int
}

func (f *fakeLSPProvider) Hover(string, int, int) (string, error) {
	return f.hoverContents, f.hoverErr
}

func (f *fakeLSPProvider) Shutdown() { f.shutdowns++ }

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

func doLSPRequest(t *testing.T, srv *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestLSPHoverHappyPath(t *testing.T) {
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

func TestLSPHoverErrors(t *testing.T) {
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
		{"absolute path", "/api/lsp/hover?path=%2Fetc%2Fpasswd.go&line=1&char=0", http.StatusBadRequest},
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
	srv, _ := newLSPTestServer(t, &fakeLSPProvider{})
	req := httptest.NewRequest(http.MethodPost, "/api/lsp/hover?path=main.go&line=1&char=0", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestLSPDisabledByConfig(t *testing.T) {
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

func TestLSPUnavailableUnderRangeFocus(t *testing.T) {
	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, sess := newLSPTestServer(t, fake)
	sess.Focus = Focus{Kind: FocusRange, BaseSHA: "b", HeadSHA: "h"}

	if w := doLSPRequest(t, srv, "/api/lsp/hover?path=main.go&line=1&char=0"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 under range focus", w.Code)
	}
	if srv.lspAvailable() {
		t.Error("lspAvailable must be false under range/PR focus (LSP reads the working tree, not Focus.HeadSHA)")
	}
}

func TestLSPConfigExposesAvailability(t *testing.T) {
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
	var c config.Config
	if !c.LSPEnabled() {
		t.Error("LSPEnabled must default to true")
	}
}
