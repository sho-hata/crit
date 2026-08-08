package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

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

	if err := fs.client.Initialize("/tmp/repo"); err != nil {
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
	fs := startFake(func(method string, params json.RawMessage) any {
		if method == "textDocument/definition" {
			return []map[string]any{
				{
					"uri": "file:///tmp/repo/util.go",
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

	locs, err := fs.client.Definition("/tmp/repo/main.go", 0, 0)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("got %d locations, want 1", len(locs))
	}
	want := Location{Path: "/tmp/repo/util.go", Line: 9, Character: 5}
	if locs[0] != want {
		t.Errorf("location = %+v, want %+v", locs[0], want)
	}
}

func TestClientAnswersServerRequests(t *testing.T) {
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

func TestClientDeadAfterTransportClose(t *testing.T) {
	fs := startFake(nil)
	fs.kill()
	waitFor(t, "client dead", fs.client.Dead)
	if _, err := fs.client.Hover("/tmp/x.go", 0, 0); err == nil {
		t.Error("Hover on dead client should error")
	}
}

func TestHoverContentsToMarkdown(t *testing.T) {
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
			if got := hoverContentsToMarkdown(json.RawMessage(tt.raw)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseLocations(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []Location
	}{
		{
			"single location",
			`{"uri":"file:///a/b.go","range":{"start":{"line":1,"character":2}}}`,
			[]Location{{Path: "/a/b.go", Line: 1, Character: 2}},
		},
		{
			"location array",
			`[{"uri":"file:///a.go","range":{"start":{"line":0,"character":0}}},{"uri":"file:///b.go","range":{"start":{"line":3,"character":4}}}]`,
			[]Location{{Path: "/a.go"}, {Path: "/b.go", Line: 3, Character: 4}},
		},
		{
			"location link array",
			`[{"targetUri":"file:///c.go","targetSelectionRange":{"start":{"line":7,"character":1}}}]`,
			[]Location{{Path: "/c.go", Line: 7, Character: 1}},
		},
		{"null", `null`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

// The Windows-only branches (drive-letter prefixes) cannot run on other
// platforms because runtime.GOOS is a constant; these tables cover the
// POSIX behavior plus the URI-shape invariants gopls relies on.
func TestPathToURI(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"absolute path", "/tmp/repo/main.go", "file:///tmp/repo/main.go"},
		{"space is percent-encoded", "/tmp/repo/sub dir/file.go", "file:///tmp/repo/sub%20dir/file.go"},
		{"plus survives, parens percent-encoded", "/tmp/a+b (c)/f.go", "file:///tmp/a+b%20%28c%29/f.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PathToURI(tt.path); got != tt.want {
				t.Errorf("PathToURI(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestURIToPath(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{"file URI", "file:///tmp/repo/main.go", "/tmp/repo/main.go"},
		{"percent-encoded space", "file:///tmp/repo/sub%20dir/file.go", "/tmp/repo/sub dir/file.go"},
		{"non-file scheme", "https://example.com/x.go", ""},
		{"unparseable URI", ":no-scheme", ""},
		{"empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := URIToPath(tt.uri); got != tt.want {
				t.Errorf("URIToPath(%q) = %q, want %q", tt.uri, got, tt.want)
			}
		})
	}
}

func TestPathURIRoundtrip(t *testing.T) {
	paths := []string{
		"/tmp/repo/main.go",
		"/tmp/repo/sub dir/file.go",
		"/tmp/リポジトリ/課題.go", // non-ASCII must survive encode/decode
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
