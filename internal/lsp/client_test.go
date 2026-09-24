package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// nativePath maps a POSIX-shaped test path to the platform's native absolute
// form (C:\tmp\repo\main.go on Windows) so path assertions hold on both.
func nativePath(p string) string {
	if runtime.GOOS == "windows" {
		return "C:" + filepath.FromSlash(p)
	}
	return p
}

// fileURI is the file:// URI a language server sends for nativePath(p). The
// argument keeps URI escaping, so pass "sub%20dir", not "sub dir".
func fileURI(p string) string {
	if runtime.GOOS == "windows" {
		return "file:///C:" + p
	}
	return "file://" + p
}

// fakeServer is an in-process LSP server wired to a Client via io.Pipe —
// no gopls subprocess involved.
type fakeServer struct {
	client *Client

	mu            sync.Mutex
	notifications []jsonrpcMessage
	responses     []jsonrpcMessage // client's answers to server->client requests

	handler func(method string, params json.RawMessage) any

	out     io.WriteCloser // server -> client
	outMu   sync.Mutex
	in      io.ReadCloser // client -> server
	nextID  int64
	reqDone map[int64]chan jsonrpcMessage
}

// startFake wires a Client to a fake server. handler produces the result for
// each client request; initialize is answered automatically when handler
// returns nil for it.
func startFake(handler func(method string, params json.RawMessage) any) *fakeServer {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	fs := &fakeServer{
		handler: handler,
		out:     s2cW,
		in:      c2sR,
		reqDone: make(map[int64]chan jsonrpcMessage),
	}
	fs.client = NewClient(c2sW, s2cR, nil)
	go fs.loop()
	return fs
}

func (fs *fakeServer) loop() {
	r := bufio.NewReader(fs.in)
	for {
		msg, err := readFrame(r)
		if err != nil {
			return
		}
		switch {
		case msg.Method != "" && msg.ID != nil: // request from client
			var result any
			if fs.handler != nil {
				result = fs.handler(msg.Method, msg.Params)
			}
			if result == nil && msg.Method == "initialize" {
				result = map[string]any{"capabilities": map[string]any{}}
			}
			fs.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		case msg.Method != "": // notification
			fs.mu.Lock()
			fs.notifications = append(fs.notifications, msg)
			fs.mu.Unlock()
		default: // response to a server->client request
			fs.mu.Lock()
			fs.responses = append(fs.responses, msg)
			fs.mu.Unlock()
		}
	}
}

func (fs *fakeServer) send(v any) {
	body, _ := json.Marshal(v)
	fs.outMu.Lock()
	defer fs.outMu.Unlock()
	fmt.Fprintf(fs.out, "Content-Length: %d\r\n\r\n", len(body))
	fs.out.Write(body)
}

// kill closes both pipes, simulating a gopls crash.
func (fs *fakeServer) kill() {
	fs.in.Close()
	fs.out.Close()
}

func (fs *fakeServer) notificationMethods() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]string, len(fs.notifications))
	for i, n := range fs.notifications {
		out[i] = n.Method
	}
	return out
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestClientInitializeAndHover(t *testing.T) {
	t.Parallel()

	fs := startFake(func(method string, params json.RawMessage) any {
		if method == "textDocument/hover" {
			var p struct {
				Position struct {
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"position"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				t.Errorf("bad hover params: %v", err)
			}
			if p.Position.Line != 4 || p.Position.Character != 7 {
				t.Errorf("position = %+v, want line 4 char 7", p.Position)
			}
			return map[string]any{
				"contents": map[string]any{"kind": "markdown", "value": "func Foo()"},
			}
		}
		return nil
	})
	defer fs.client.Close()

	if err := fs.client.Initialize("/tmp/repo", nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	got, err := fs.client.Hover("/tmp/repo/main.go", 4, 7)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got != "func Foo()" {
		t.Errorf("Hover = %q, want %q", got, "func Foo()")
	}
	waitFor(t, "initialized notification", func() bool {
		for _, m := range fs.notificationMethods() {
			if m == "initialized" {
				return true
			}
		}
		return false
	})
}

func TestClientDefinition(t *testing.T) {
	t.Parallel()

	fs := startFake(func(method string, params json.RawMessage) any {
		if method == "textDocument/definition" {
			return []map[string]any{
				{
					"uri": fileURI("/tmp/repo/util.go"),
					"range": map[string]any{
						"start": map[string]any{"line": 9, "character": 5},
						"end":   map[string]any{"line": 9, "character": 12},
					},
				},
			}
		}
		return nil
	})
	defer fs.client.Close()

	locs, err := fs.client.Definition(nativePath("/tmp/repo/main.go"), 0, 0)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("got %d locations, want 1", len(locs))
	}
	want := Location{Path: nativePath("/tmp/repo/util.go"), Line: 9, Character: 5}
	if locs[0] != want {
		t.Errorf("location = %+v, want %+v", locs[0], want)
	}
}

func TestClientReferences(t *testing.T) {
	t.Parallel()

	fs := startFake(func(method string, params json.RawMessage) any {
		if method == "textDocument/references" {
			var p struct {
				Context struct {
					IncludeDeclaration bool `json:"includeDeclaration"`
				} `json:"context"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				t.Errorf("bad references params: %v", err)
			}
			if !p.Context.IncludeDeclaration {
				t.Error("references must request includeDeclaration")
			}
			return []map[string]any{
				{
					"uri":   fileURI("/tmp/repo/main.go"),
					"range": map[string]any{"start": map[string]any{"line": 4, "character": 1}},
				},
				{
					"uri":   fileURI("/tmp/repo/util.go"),
					"range": map[string]any{"start": map[string]any{"line": 9, "character": 5}},
				},
			}
		}
		return nil
	})
	defer fs.client.Close()

	locs, err := fs.client.References(nativePath("/tmp/repo/main.go"), 4, 1)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	want := []Location{
		{Path: nativePath("/tmp/repo/main.go"), Line: 4, Character: 1},
		{Path: nativePath("/tmp/repo/util.go"), Line: 9, Character: 5},
	}
	if len(locs) != len(want) {
		t.Fatalf("got %d locations, want %d", len(locs), len(want))
	}
	for i := range locs {
		if locs[i] != want[i] {
			t.Errorf("location[%d] = %+v, want %+v", i, locs[i], want[i])
		}
	}
}

func TestClientAnswersServerRequests(t *testing.T) {
	t.Parallel()

	fs := startFake(nil)
	defer fs.client.Close()

	// Push a workspace/configuration request at the client; it must answer
	// with one null per item so gopls never blocks on us.
	id := json.RawMessage(`99`)
	fs.send(map[string]any{
		"jsonrpc": "2.0", "id": &id, "method": "workspace/configuration",
		"params": map[string]any{"items": []any{map[string]any{}, map[string]any{}}},
	})
	waitFor(t, "configuration response", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.responses) == 1
	})
	fs.mu.Lock()
	resp := fs.responses[0]
	fs.mu.Unlock()
	var result []any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("parsing response result: %v", err)
	}
	if len(result) != 2 || result[0] != nil || result[1] != nil {
		t.Errorf("configuration response = %v, want [null null]", result)
	}
}

// The capability is what makes a server pull configuration at all, and only a
// language with settings may declare it: a server with none must not be sent
// a capability it will never use.
func TestClientInitializeDeclaresConfigurationOnlyWithSettings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		settings map[string]any
		want     bool
	}{
		{"no settings", nil, false},
		{"empty settings", map[string]any{}, false},
		{"settings", map[string]any{"python": map[string]any{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var caps map[string]any
			fs := startFake(func(method string, params json.RawMessage) any {
				if method == "initialize" {
					var p struct {
						Capabilities map[string]any `json:"capabilities"`
					}
					if err := json.Unmarshal(params, &p); err != nil {
						t.Errorf("bad initialize params: %v", err)
					}
					caps = p.Capabilities
				}
				return nil
			})
			defer fs.client.Close()
			fs.client.SetSettings(tc.settings)
			if err := fs.client.Initialize("/tmp/repo", nil); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			ws, declared := caps["workspace"].(map[string]any)
			if declared != tc.want {
				t.Fatalf("workspace capability declared = %v, want %v (caps=%v)", declared, tc.want, caps)
			}
			if tc.want && ws["configuration"] != true {
				t.Errorf("workspace.configuration = %v, want true", ws["configuration"])
			}
		})
	}
}

// workspace/configuration is answered from the language's settings, one entry
// per requested item and in the same order; an item it has nothing for gets
// null, which the server reads as "use your default".
func TestClientAnswersConfigurationFromSettings(t *testing.T) {
	t.Parallel()

	fs := startFake(nil)
	defer fs.client.Close()
	extra := []string{"/venv/lib/python3.13/site-packages"}
	fs.client.SetSettings(map[string]any{
		"python": map[string]any{"analysis": map[string]any{"extraPaths": extra}},
	})

	id := json.RawMessage(`7`)
	fs.send(map[string]any{
		"jsonrpc": "2.0", "id": &id, "method": "workspace/configuration",
		"params": map[string]any{"items": []any{
			map[string]any{"section": "python"},
			map[string]any{"section": "pyright"},
			map[string]any{"section": "python.analysis"},
			map[string]any{}, // no section: the whole tree, which we do not serve
		}},
	})
	waitFor(t, "configuration response", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.responses) == 1
	})
	fs.mu.Lock()
	resp := fs.responses[0]
	fs.mu.Unlock()
	var result []any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("parsing response result: %v", err)
	}
	if len(result) != 4 {
		t.Fatalf("got %d entries, want 4 (one per item): %v", len(result), result)
	}
	analysis := map[string]any{"extraPaths": []any{extra[0]}}
	if got, want := result[0], any(map[string]any{"analysis": analysis}); !reflect.DeepEqual(got, want) {
		t.Errorf("python = %v, want %v", got, want)
	}
	if result[1] != nil {
		t.Errorf("pyright = %v, want null", result[1])
	}
	if got := result[2]; !reflect.DeepEqual(got, any(analysis)) {
		t.Errorf("python.analysis = %v, want %v", got, analysis)
	}
	if result[3] != nil {
		t.Errorf("sectionless item = %v, want null", result[3])
	}
}

func TestLookupSection(t *testing.T) {
	t.Parallel()

	settings := map[string]any{
		"python": map[string]any{
			"analysis": map[string]any{"extraPaths": []string{"/sp"}},
			"leaf":     "text",
		},
		"gopls.ui": "flat key with a dot",
	}
	cases := []struct {
		name     string
		settings map[string]any
		section  string
		want     any
	}{
		{"top level", settings, "python", settings["python"]},
		{"dotted path", settings, "python.analysis", settings["python"].(map[string]any)["analysis"]},
		{"leaf value", settings, "python.leaf", "text"},
		{"exact key beats the dotted walk", settings, "gopls.ui", "flat key with a dot"},
		{"missing section", settings, "pyright", nil},
		{"missing nested key", settings, "python.nope", nil},
		{"path through a non-map", settings, "python.leaf.deeper", nil},
		{"empty section", settings, "", nil},
		{"nil settings", nil, "python", nil},
	}
	for _, tc := range cases {
		if got := lookupSection(tc.settings, tc.section); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: lookupSection(%q) = %v, want %v", tc.name, tc.section, got, tc.want)
		}
	}
}

func TestClientDeadAfterTransportClose(t *testing.T) {
	t.Parallel()

	fs := startFake(nil)
	fs.kill()
	waitFor(t, "client dead", fs.client.Dead)
	if _, err := fs.client.Hover("/tmp/x.go", 0, 0); err == nil {
		t.Error("Hover on dead client should error")
	}
}

func TestHoverContentsToMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"markup content", `{"kind":"markdown","value":"**doc**"}`, "**doc**"},
		{"bare string", `"plain"`, "plain"},
		{"marked string with language", `{"language":"go","value":"func F()"}`, "```go\nfunc F()\n```"},
		{"array", `[{"language":"go","value":"func F()"},"desc"]`, "```go\nfunc F()\n```\n\ndesc"},
		{"null", `null`, ""},
		{"empty", ``, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := hoverContentsToMarkdown(json.RawMessage(tt.raw)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseLocations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want []Location
	}{
		{
			"single location",
			`{"uri":"` + fileURI("/a/b.go") + `","range":{"start":{"line":1,"character":2}}}`,
			[]Location{{Path: nativePath("/a/b.go"), Line: 1, Character: 2}},
		},
		{
			"location array",
			`[{"uri":"` + fileURI("/a.go") + `","range":{"start":{"line":0,"character":0}}},{"uri":"` + fileURI("/b.go") + `","range":{"start":{"line":3,"character":4}}}]`,
			[]Location{{Path: nativePath("/a.go")}, {Path: nativePath("/b.go"), Line: 3, Character: 4}},
		},
		{
			"location link array",
			`[{"targetUri":"` + fileURI("/c.go") + `","targetSelectionRange":{"start":{"line":7,"character":1}}}]`,
			[]Location{{Path: nativePath("/c.go"), Line: 7, Character: 1}},
		},
		{"null", `null`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parseLocations(json.RawMessage(tt.raw))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d locations, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("location[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// nativePath/fileURI keep these tables platform-agnostic: on Windows they
// exercise the drive-letter branches, elsewhere the POSIX ones, and both cover
// the URI-shape invariants gopls relies on.
func TestPathToURI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{"absolute path", nativePath("/tmp/repo/main.go"), fileURI("/tmp/repo/main.go")},
		{"space is percent-encoded", nativePath("/tmp/repo/sub dir/file.go"), fileURI("/tmp/repo/sub%20dir/file.go")},
		{"plus survives, parens percent-encoded", nativePath("/tmp/a+b (c)/f.go"), fileURI("/tmp/a+b%20%28c%29/f.go")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := PathToURI(tt.path); got != tt.want {
				t.Errorf("PathToURI(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestURIToPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		uri  string
		want string
	}{
		{"file URI", fileURI("/tmp/repo/main.go"), nativePath("/tmp/repo/main.go")},
		{"percent-encoded space", fileURI("/tmp/repo/sub%20dir/file.go"), nativePath("/tmp/repo/sub dir/file.go")},
		{"non-file scheme", "https://example.com/x.go", ""},
		{"unparseable URI", ":no-scheme", ""},
		{"empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := URIToPath(tt.uri); got != tt.want {
				t.Errorf("URIToPath(%q) = %q, want %q", tt.uri, got, tt.want)
			}
		})
	}
}

func TestPathURIRoundtrip(t *testing.T) {
	t.Parallel()

	paths := []string{
		nativePath("/tmp/repo/main.go"),
		nativePath("/tmp/repo/sub dir/file.go"),
		nativePath("/tmp/リポジトリ/課題.go"), // non-ASCII must survive encode/decode
	}
	for _, path := range paths {
		uri := PathToURI(path)
		if !strings.HasPrefix(uri, "file://") {
			t.Fatalf("PathToURI(%q) = %q, want file:// prefix", path, uri)
		}
		if got := URIToPath(uri); got != path {
			t.Errorf("roundtrip(%q) = %q", path, got)
		}
	}
}

// sendProgress emits one work-done progress notification from the fake server.
func (fs *fakeServer) sendProgress(token, kind string) {
	fs.send(map[string]any{"jsonrpc": "2.0", "method": "$/progress", "params": map[string]any{
		"token": token,
		"value": map[string]any{"kind": kind, "title": "Initializing JS/TS language features…"},
	}})
}

func TestInitializeOptsIntoWorkDoneProgress(t *testing.T) {
	t.Parallel()

	var caps struct {
		Window struct {
			WorkDoneProgress bool `json:"workDoneProgress"`
		} `json:"window"`
	}
	fs := startFake(func(method string, params json.RawMessage) any {
		if method == "initialize" {
			var p struct {
				Capabilities json.RawMessage `json:"capabilities"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				t.Errorf("bad initialize params: %v", err)
			}
			if err := json.Unmarshal(p.Capabilities, &caps); err != nil {
				t.Errorf("bad capabilities: %v", err)
			}
		}
		return nil
	})
	defer fs.client.Close()

	if err := fs.client.Initialize("/tmp/repo", nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// Without this capability the server never reports its startup work and
	// WaitReady has nothing to wait on — the first answer goes back wrong.
	if !caps.Window.WorkDoneProgress {
		t.Error("initialize must declare window.workDoneProgress")
	}
}

func TestInitializeSendsInitializationOptions(t *testing.T) {
	t.Parallel()

	var got map[string]any
	fs := startFake(func(method string, params json.RawMessage) any {
		if method == "initialize" {
			var p struct {
				InitializationOptions map[string]any `json:"initializationOptions"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				t.Errorf("bad initialize params: %v", err)
			}
			got = p.InitializationOptions
		}
		return nil
	})
	defer fs.client.Close()

	opts := map[string]any{"tsserver": map[string]any{"path": "/repo/node_modules/typescript/lib/tsserver.js"}}
	if err := fs.client.Initialize("/tmp/repo", opts); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	ts, ok := got["tsserver"].(map[string]any)
	if !ok {
		t.Fatalf("initializationOptions = %v, want a tsserver entry", got)
	}
	if ts["path"] != "/repo/node_modules/typescript/lib/tsserver.js" {
		t.Errorf("tsserver.path = %v", ts["path"])
	}
}

func TestWaitReadyWaitsOutReportedWork(t *testing.T) {
	t.Parallel()

	fs := startFake(nil)
	defer fs.client.Close()
	if err := fs.client.Initialize("/tmp/repo", nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	fs.sendProgress("tok-1", "begin")

	done := make(chan struct{})
	go func() {
		fs.client.WaitReady(time.Second, 2*time.Second)
		close(done)
	}()

	// The whole point: no request may go out while the project is loading,
	// because the server would answer it with the wrong location.
	select {
	case <-done:
		t.Fatal("WaitReady returned while work was still in progress")
	case <-time.After(100 * time.Millisecond):
	}

	fs.sendProgress("tok-1", "end")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitReady did not return after the work ended")
	}
}

func TestWaitReadyProceedsWhenServerReportsNothing(t *testing.T) {
	t.Parallel()

	fs := startFake(nil)
	defer fs.client.Close()
	if err := fs.client.Initialize("/tmp/repo", nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// A server that reports no progress at all (or does not implement it)
	// must cost only the grace window, not the full timeout.
	start := time.Now()
	fs.client.WaitReady(50*time.Millisecond, 10*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("WaitReady blocked for %s, want ~the 50ms grace", elapsed)
	}
}

func TestWaitReadyReturnsWhenServerDies(t *testing.T) {
	t.Parallel()

	fs := startFake(nil)
	if err := fs.client.Initialize("/tmp/repo", nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	fs.sendProgress("tok-1", "begin")
	waitFor(t, "progress begin to land", func() bool {
		seen, _ := fs.client.progressState()
		return seen
	})

	done := make(chan struct{})
	go func() {
		fs.client.WaitReady(time.Second, 10*time.Second)
		close(done)
	}()
	fs.kill() // crash mid-load: waiting out a dead server's work is pointless

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WaitReady did not return after the server died")
	}
}
