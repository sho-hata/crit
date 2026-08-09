package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tomasz-tomczyk/crit/internal/config"
	"github.com/tomasz-tomczyk/crit/internal/lsp"
)

// fakeLSPProvider implements lspProvider without spawning gopls.
type fakeLSPProvider struct {
	hoverContents string
	hoverErr      error
	locations     []lsp.Location
	definitionErr error
	references    []lsp.Location
	referencesErr error
	goroot        string
	gomodcache    string
	shutdowns     int
}

func (f *fakeLSPProvider) Hover(string, int, int) (string, error) {
	return f.hoverContents, f.hoverErr
}

func (f *fakeLSPProvider) Definition(string, int, int) ([]lsp.Location, error) {
	return f.locations, f.definitionErr
}

func (f *fakeLSPProvider) References(string, int, int) ([]lsp.Location, error) {
	return f.references, f.referencesErr
}

func (f *fakeLSPProvider) GoEnv() (string, string) { return f.goroot, f.gomodcache }
func (f *fakeLSPProvider) Shutdown()               { f.shutdowns++ }

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
		// Absolute paths are allowed for chained peek jumps but scoped to
		// repo root / GOROOT / GOMODCACHE — anything else is denied.
		{"absolute path outside roots", "/api/lsp/hover?path=%2Fetc%2Fpasswd.go&line=1&char=0", http.StatusForbidden},
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

func TestLSPDefinitionInSessionAndPeek(t *testing.T) {
	srv, sess := newLSPTestServer(t, nil)
	fake := &fakeLSPProvider{
		locations: []lsp.Location{
			{Path: filepath.Join(sess.RepoRoot, "main.go"), Line: 2, Character: 5},
		},
	}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Locations) != 1 {
		t.Fatalf("got %d locations, want 1", len(resp.Locations))
	}
	loc := resp.Locations[0]
	if loc.Path != "main.go" || !loc.InRepo || !loc.InSession {
		t.Errorf("location = %+v; want in-repo in-session main.go", loc)
	}
	if loc.Line != 3 {
		t.Errorf("line = %d, want 3 (1-based)", loc.Line)
	}
	if loc.PeekStart != 1 || len(loc.Peek) == 0 {
		t.Errorf("peek = start %d, %d lines; want start 1 with content", loc.PeekStart, len(loc.Peek))
	}
	if !strings.Contains(strings.Join(loc.Peek, "\n"), "func main()") {
		t.Errorf("peek missing target content: %v", loc.Peek)
	}
}

func TestLSPDefinitionInRepoNotInSession(t *testing.T) {
	srv, sess := newLSPTestServer(t, nil)
	otherPath := filepath.Join(sess.RepoRoot, "helper.go")
	if err := os.WriteFile(otherPath, []byte("package main\n\nfunc helper() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLSPProvider{locations: []lsp.Location{{Path: otherPath, Line: 2}}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if !loc.InRepo || loc.InSession {
		t.Errorf("location = %+v; want in-repo, NOT in-session", loc)
	}
	if len(loc.Peek) == 0 {
		t.Error("in-repo target must carry a peek")
	}
}

func TestLSPDefinitionOutsideAllowedRootsHasNoPeek(t *testing.T) {
	srv, _ := newLSPTestServer(t, nil)
	outside := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(outside, []byte("package secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// goroot/gomodcache empty: the outside dir matches no allowed root.
	fake := &fakeLSPProvider{locations: []lsp.Location{{Path: outside, Line: 0}}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if loc.InRepo || loc.InSession {
		t.Errorf("location = %+v; want outside repo/session", loc)
	}
	if len(loc.Peek) != 0 {
		t.Error("peek must be withheld for paths outside repo/GOROOT/GOMODCACHE")
	}
}

func TestTruncateLineKeepsRuneBoundary(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"ascii", strings.Repeat("a", peekMaxLineLen+100)},
		// Multi-byte runes positioned so a naive byte slice would cut mid-rune.
		{"multibyte", strings.Repeat("あ", peekMaxLineLen)},
		{"mixed", strings.Repeat("a", peekMaxLineLen-1) + strings.Repeat("識", 50)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateLine(tt.line)
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("long line must end with ellipsis, got %q…", got[len(got)-10:])
			}
			if !utf8.ValidString(got) {
				t.Error("truncation split a multi-byte rune")
			}
			if len(got) > peekMaxLineLen+len("…") {
				t.Errorf("truncated to %d bytes, want <= %d", len(got), peekMaxLineLen+len("…"))
			}
		})
	}
	short := "short あいう line"
	if got := truncateLine(short); got != short {
		t.Errorf("short line must be unchanged, got %q", got)
	}
}

func TestReadPeekBoundaries(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, lineCount int) string {
		path := filepath.Join(dir, name)
		var b strings.Builder
		for i := 0; i < lineCount; i++ {
			fmt.Fprintf(&b, "line %d\n", i+1)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// A trailing newline must not produce a phantom empty line past EOF.
	small := write("small.go", 3)
	start, lines, truncated := readPeek(small, 2, peekFullFileMaxLines, peekContextLines)
	if start != 1 || truncated || len(lines) != 3 {
		t.Errorf("small file: start=%d truncated=%v lines=%d, want 1/false/3", start, truncated, len(lines))
	}

	// Exactly peekFullFileMaxLines lines (newline-terminated) is still a
	// whole-file peek, not a truncated window.
	exact := write("exact.go", peekFullFileMaxLines)
	start, lines, truncated = readPeek(exact, 1000, peekFullFileMaxLines, peekContextLines)
	if start != 1 || truncated || len(lines) != peekFullFileMaxLines {
		t.Errorf("exact-limit file: start=%d truncated=%v lines=%d, want 1/false/%d",
			start, truncated, len(lines), peekFullFileMaxLines)
	}

	// One line over the limit tips it into the ±peekContextLines window.
	over := write("over.go", peekFullFileMaxLines+1)
	start, lines, truncated = readPeek(over, 1000, peekFullFileMaxLines, peekContextLines)
	if !truncated || start != 1000-peekContextLines || len(lines) != 2*peekContextLines+1 {
		t.Errorf("over-limit file: start=%d truncated=%v lines=%d, want %d/true/%d",
			start, truncated, len(lines), 1000-peekContextLines, 2*peekContextLines+1)
	}
}

func TestLSPDefinitionPeekTruncatesLargeFiles(t *testing.T) {
	srv, sess := newLSPTestServer(t, nil)
	bigPath := filepath.Join(sess.RepoRoot, "generated.go")
	var b strings.Builder
	b.WriteString("package main\n")
	for i := 0; i < 3000; i++ {
		b.WriteString("// filler line\n")
	}
	if err := os.WriteFile(bigPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLSPProvider{locations: []lsp.Location{{Path: bigPath, Line: 1500}}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if !loc.PeekTruncated {
		t.Error("large file peek must set peek_truncated")
	}
	if len(loc.Peek) != 2*100+1 {
		t.Errorf("windowed peek = %d lines, want %d", len(loc.Peek), 2*100+1)
	}
	if loc.PeekStart != 1501-100 {
		t.Errorf("peek_start = %d, want %d", loc.PeekStart, 1501-100)
	}

	// Small files come back whole and untruncated. Fresh response struct:
	// peek_truncated is omitempty, and json.Unmarshal into a reused struct
	// would keep the previous true.
	fake.locations = []lsp.Location{{Path: filepath.Join(sess.RepoRoot, "main.go"), Line: 0}}
	w = doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp2 struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.Locations[0].PeekTruncated {
		t.Error("small file peek must not be truncated")
	}
	if resp2.Locations[0].PeekStart != 1 {
		t.Errorf("small file peek_start = %d, want 1 (whole file)", resp2.Locations[0].PeekStart)
	}
}

func TestLSPDefinitionGorootPeek(t *testing.T) {
	srv, _ := newLSPTestServer(t, nil)
	goroot := t.TempDir()
	stdlibFile := filepath.Join(goroot, "src", "fmt", "print.go")
	if err := os.MkdirAll(filepath.Dir(stdlibFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stdlibFile, []byte("package fmt\n\nfunc Println() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLSPProvider{
		locations: []lsp.Location{{Path: stdlibFile, Line: 2}},
		goroot:    goroot,
	}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/definition?path=main.go&line=1&char=0")
	var resp struct {
		Locations []lspLocationResponse `json:"locations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if loc.InRepo {
		t.Errorf("stdlib target must not be in-repo: %+v", loc)
	}
	if loc.DisplayPath != "$GOROOT/src/fmt/print.go" {
		t.Errorf("display_path = %q", loc.DisplayPath)
	}
	if len(loc.Peek) == 0 {
		t.Error("GOROOT target must carry a peek")
	}
}

// TestLSPAbsolutePathScoping covers chained jumps from the peek popup:
// absolute paths are accepted only under repo root / GOROOT / GOMODCACHE.
func TestLSPAbsolutePathScoping(t *testing.T) {
	fake := &fakeLSPProvider{hoverContents: "doc"}
	srv, sess := newLSPTestServer(t, fake)

	goroot := t.TempDir()
	stdlibFile := filepath.Join(goroot, "src", "fmt", "print.go")
	if err := os.MkdirAll(filepath.Dir(stdlibFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stdlibFile, []byte("package fmt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.goroot = goroot

	outside := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(outside, []byte("package secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		code int
	}{
		{"absolute in repo", filepath.Join(sess.RepoRoot, "main.go"), http.StatusOK},
		{"absolute in GOROOT", stdlibFile, http.StatusOK},
		{"absolute outside allowed roots", outside, http.StatusForbidden},
		{"absolute traversal into repo stays allowed after Clean", sess.RepoRoot + "/sub/../main.go", http.StatusOK},
		{"absolute traversal escaping repo", sess.RepoRoot + "/../escape.go", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doLSPRequest(t, srv, "/api/lsp/hover?path="+url.QueryEscape(tt.path)+"&line=1&char=0")
			if w.Code != tt.code {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tt.code, w.Body.String())
			}
		})
	}
}

// lspReferencesResponse mirrors the /api/lsp/references payload in tests.
type lspReferencesResponse struct {
	Locations []lspLocationResponse `json:"locations"`
	Truncated bool                  `json:"truncated"`
}

func TestLSPReferencesSortedWithSnippetPeek(t *testing.T) {
	srv, sess := newLSPTestServer(t, nil)
	helperPath := filepath.Join(sess.RepoRoot, "helper.go")
	if err := os.WriteFile(helperPath, []byte("package main\n\nfunc helper() { main() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// helper.go sorts first alphabetically but is not part of the review, so
	// the in-session main.go must come first.
	fake := &fakeLSPProvider{references: []lsp.Location{
		{Path: helperPath, Line: 2, Character: 16},
		{Path: filepath.Join(sess.RepoRoot, "main.go"), Line: 2, Character: 5},
	}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=3&char=6")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp lspReferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Truncated {
		t.Error("truncated must be false for a small reference list")
	}
	if len(resp.Locations) != 2 {
		t.Fatalf("got %d locations, want 2", len(resp.Locations))
	}
	if resp.Locations[0].Path != "main.go" || resp.Locations[1].Path != "helper.go" {
		t.Errorf("in-session file must sort first, got %q then %q",
			resp.Locations[0].Path, resp.Locations[1].Path)
	}
	ref := resp.Locations[0]
	if !ref.InRepo || !ref.InSession || ref.Line != 3 {
		t.Errorf("main.go reference = %+v; want in-repo in-session line 3", ref)
	}
	// Small files come back whole; the reference's own line must be inside
	// the peek window so the UI can render a snippet row.
	idx := ref.Line - ref.PeekStart
	if idx < 0 || idx >= len(ref.Peek) {
		t.Fatalf("reference line %d outside peek window start=%d len=%d", ref.Line, ref.PeekStart, len(ref.Peek))
	}
	if !strings.Contains(ref.Peek[idx], "func main()") {
		t.Errorf("snippet = %q, want the reference line", ref.Peek[idx])
	}
}

// TestLSPReferencesCapKeepsRelevantFiles covers the interaction between the
// relevance sort and the maxReferenceLocations cap: when more references
// exist than fit, the ones in files under review must survive even though
// their paths sort last alphabetically.
func TestLSPReferencesCapKeepsRelevantFiles(t *testing.T) {
	srv, sess := newLSPTestServer(t, nil)
	// aaa.go is in the repo but not under review, and sorts before main.go.
	bulkPath := filepath.Join(sess.RepoRoot, "aaa.go")
	if err := os.WriteFile(bulkPath, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs := make([]lsp.Location, 0, maxReferenceLocations+10)
	for i := 0; i < maxReferenceLocations+5; i++ {
		refs = append(refs, lsp.Location{Path: bulkPath, Line: i})
	}
	refs = append(refs, lsp.Location{Path: filepath.Join(sess.RepoRoot, "main.go"), Line: 2})
	fake := &fakeLSPProvider{references: refs}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=3&char=6")
	var resp lspReferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || len(resp.Locations) != maxReferenceLocations {
		t.Fatalf("truncated=%v with %d locations, want true with %d",
			resp.Truncated, len(resp.Locations), maxReferenceLocations)
	}
	if resp.Locations[0].Path != "main.go" {
		t.Errorf("first location = %q, want the in-session main.go to survive the cap",
			resp.Locations[0].Path)
	}
}

// TestReferenceCharacterTiebreak pins the character tiebreak: two references
// on the same line must have a deterministic order.
func TestReferenceCharacterTiebreak(t *testing.T) {
	_, sess := newLSPTestServer(t, nil)
	path := filepath.Join(sess.RepoRoot, "main.go")
	locs := []lsp.Location{
		{Path: path, Line: 4, Character: 20},
		{Path: path, Line: 4, Character: 3},
	}
	rc := newRootCache(sess.RepoRoot, "", "")
	sortReferences(locs, sess, rc)
	if locs[0].Character != 3 || locs[1].Character != 20 {
		t.Errorf("same-line references not ordered by character: %+v", locs)
	}
}

func TestLSPReferencesSmallPeekWindow(t *testing.T) {
	srv, sess := newLSPTestServer(t, nil)
	bigPath := filepath.Join(sess.RepoRoot, "big.go")
	var b strings.Builder
	b.WriteString("package main\n")
	for i := 0; i < 100; i++ {
		b.WriteString("// filler line\n")
	}
	if err := os.WriteFile(bigPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLSPProvider{references: []lsp.Location{{Path: bigPath, Line: 50}}}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=1&char=0")
	var resp lspReferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	loc := resp.Locations[0]
	if !loc.PeekTruncated {
		t.Error("reference peek in a >50-line file must be windowed")
	}
	if len(loc.Peek) != 2*10+1 {
		t.Errorf("reference peek = %d lines, want %d (±10 window)", len(loc.Peek), 2*10+1)
	}
}

func TestLSPReferencesTruncatesLongLists(t *testing.T) {
	srv, sess := newLSPTestServer(t, nil)
	mainPath := filepath.Join(sess.RepoRoot, "main.go")
	refs := make([]lsp.Location, maxReferenceLocations+50)
	for i := range refs {
		refs[i] = lsp.Location{Path: mainPath, Line: i % 3}
	}
	fake := &fakeLSPProvider{references: refs}
	srv.lsp.newProvider = func() lspProvider { return fake }

	w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=1&char=0")
	var resp lspReferencesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated {
		t.Error("truncated must be true when the list is capped")
	}
	if len(resp.Locations) != maxReferenceLocations {
		t.Errorf("got %d locations, want %d", len(resp.Locations), maxReferenceLocations)
	}
}

func TestLSPReferencesProviderError(t *testing.T) {
	fake := &fakeLSPProvider{referencesErr: os.ErrDeadlineExceeded}
	srv, _ := newLSPTestServer(t, fake)

	if w := doLSPRequest(t, srv, "/api/lsp/references?path=main.go&line=1&char=0"); w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestLSPReferencesMethodNotAllowed(t *testing.T) {
	srv, _ := newLSPTestServer(t, &fakeLSPProvider{})
	req := httptest.NewRequest(http.MethodPost, "/api/lsp/references?path=main.go&line=1&char=0", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
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
