// Package lsp implements a minimal Language Server Protocol client used to
// provide hover, go-to-definition, and find-references in the review UI. It
// speaks just enough LSP (initialize, didOpen/didChange, hover, definition,
// references, shutdown) to drive gopls over stdio; it is not a
// general-purpose LSP library.
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// requestTimeout bounds a single LSP request round-trip. The first hover on a
// large module can take several seconds while gopls loads packages.
const requestTimeout = 15 * time.Second

// jsonrpcMessage is one incoming JSON-RPC 2.0 message. A message with both
// Method and ID is a server->client request; Method without ID is a
// notification; ID without Method is a response to one of our requests.
type jsonrpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *ResponseError   `json:"error,omitempty"`
}

// ResponseError is a JSON-RPC error object.
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *ResponseError) Error() string { return fmt.Sprintf("lsp: %s (code %d)", e.Message, e.Code) }

// Client is a JSON-RPC 2.0 client over a stdio-style transport.
type Client struct {
	stdin   io.WriteCloser
	writeMu sync.Mutex // serializes frame writes to stdin

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan jsonrpcMessage

	// done is closed exactly once — via doneOnce — when the transport dies:
	// either the reader loop exits (gopls crashed / closed its stdout) or
	// Close tears the client down, whichever happens first. doneOnce makes
	// those two paths safe to race: close(done) would panic on a second
	// call. A closed done makes Dead() report true and unblocks every
	// in-flight call() waiting in its select, so no request hangs on a dead
	// server.
	done     chan struct{}
	doneOnce sync.Once

	// kill terminates the underlying subprocess. Nil for in-memory pipe
	// transports (tests).
	kill func()
}

// NewClient wraps an LSP server reachable via the given pipes and starts the
// reader loop. kill, when non-nil, force-terminates the server process; it is
// invoked from Close after the polite shutdown handshake.
func NewClient(stdin io.WriteCloser, stdout io.Reader, kill func()) *Client {
	c := &Client{
		stdin:   stdin,
		pending: make(map[int64]chan jsonrpcMessage),
		done:    make(chan struct{}),
		kill:    kill,
	}
	go c.readLoop(stdout)
	return c
}

// Dead reports whether the transport has closed (server exited or crashed).
func (c *Client) Dead() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// readLoop parses Content-Length framed messages until the transport closes.
func (c *Client) readLoop(stdout io.Reader) {
	defer c.markDone()
	r := bufio.NewReader(stdout)
	for {
		msg, err := readFrame(r)
		if err != nil {
			return
		}
		c.dispatch(msg)
	}
}

func (c *Client) markDone() {
	c.doneOnce.Do(func() { close(c.done) })
	// Unblock all in-flight calls.
	c.mu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// readFrame reads one Content-Length framed JSON-RPC message.
func readFrame(r *bufio.Reader) (jsonrpcMessage, error) {
	var msg jsonrpcMessage
	contentLen := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return msg, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &contentLen); err != nil {
				return msg, fmt.Errorf("lsp: bad Content-Length %q: %w", v, err)
			}
		}
	}
	if contentLen < 0 {
		return msg, fmt.Errorf("lsp: missing Content-Length header")
	}
	body := make([]byte, contentLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return msg, err
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return msg, fmt.Errorf("lsp: parsing message: %w", err)
	}
	return msg, nil
}

// dispatch routes one incoming message: responses to their waiting caller,
// server->client requests to a default responder, notifications to /dev/null.
func (c *Client) dispatch(msg jsonrpcMessage) {
	if msg.Method != "" {
		if msg.ID != nil {
			c.respondToServerRequest(msg)
		}
		// Notifications (window/showMessage, $/progress, publishDiagnostics,
		// ...) are intentionally ignored.
		return
	}
	if msg.ID == nil {
		return
	}
	var id int64
	if err := json.Unmarshal(*msg.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if ok {
		ch <- msg
	}
}

// respondToServerRequest answers server->client requests with a minimal
// default so gopls never blocks waiting on us. workspace/configuration gets a
// null per requested item; everything else gets a null result.
func (c *Client) respondToServerRequest(msg jsonrpcMessage) {
	var result any
	if msg.Method == "workspace/configuration" {
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		result = make([]any, len(params.Items))
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result}
	_ = c.writeFrame(resp)
}

func (c *Client) writeFrame(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.stdin.Write(body)
	return err
}

// call performs a request and unmarshals the result into out (skipped when
// out is nil or the result is null).
func (c *Client) call(method string, params any, out any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan jsonrpcMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := c.writeFrame(req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("lsp: writing %s: %w", method, err)
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return fmt.Errorf("lsp: server exited during %s", method)
		}
		if msg.Error != nil {
			return msg.Error
		}
		if out == nil || len(msg.Result) == 0 || string(msg.Result) == "null" {
			return nil
		}
		return json.Unmarshal(msg.Result, out)
	case <-time.After(requestTimeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("lsp: %s timed out after %s", method, requestTimeout)
	case <-c.done:
		return fmt.Errorf("lsp: server exited during %s", method)
	}
}

func (c *Client) notify(method string, params any) error {
	return c.writeFrame(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Initialize performs the LSP initialize handshake for the given workspace
// root.
func (c *Client) Initialize(rootDir string) error {
	rootURI := PathToURI(rootDir)
	params := map[string]any{
		"processId": nil,
		"rootUri":   rootURI,
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": filepath.Base(rootDir)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"hover": map[string]any{
					"contentFormat": []string{"markdown", "plaintext"},
				},
				"definition": map[string]any{},
				"references": map[string]any{},
			},
		},
	}
	if err := c.call("initialize", params, nil); err != nil {
		return err
	}
	return c.notify("initialized", map[string]any{})
}

// DidOpen tells the server a document is open with the given content.
// languageID is the LSP language identifier (e.g. "go").
func (c *Client) DidOpen(path, languageID, text string, version int) error {
	return c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        PathToURI(path),
			"languageId": languageID,
			"version":    version,
			"text":       text,
		},
	})
}

// DidChange sends a full-content update for an open document.
func (c *Client) DidChange(path, text string, version int) error {
	return c.notify("textDocument/didChange", map[string]any{
		"textDocument": map[string]any{
			"uri":     PathToURI(path),
			"version": version,
		},
		"contentChanges": []map[string]any{{"text": text}},
	})
}

// Hover returns hover contents as markdown for a 0-based UTF-16 position, or
// "" when the server has nothing to show.
func (c *Client) Hover(path string, line, character int) (string, error) {
	var result struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := c.call("textDocument/hover", positionParams(path, line, character), &result); err != nil {
		return "", err
	}
	return hoverContentsToMarkdown(result.Contents), nil
}

// Location is a definition target. Line and Character are 0-based (UTF-16).
type Location struct {
	Path      string
	Line      int
	Character int
}

// Definition returns definition locations for a 0-based UTF-16 position.
func (c *Client) Definition(path string, line, character int) ([]Location, error) {
	var raw json.RawMessage
	if err := c.call("textDocument/definition", positionParams(path, line, character), &raw); err != nil {
		return nil, err
	}
	return parseLocations(raw), nil
}

// References returns all reference locations (including the declaration).
func (c *Client) References(path string, line, character int) ([]Location, error) {
	params := positionParams(path, line, character)
	params["context"] = map[string]any{"includeDeclaration": true}
	var raw json.RawMessage
	if err := c.call("textDocument/references", params, &raw); err != nil {
		return nil, err
	}
	return parseLocations(raw), nil
}

// Close performs the polite shutdown handshake, then force-terminates the
// server if it does not exit promptly.
func (c *Client) Close() {
	// Best effort: gopls exits on its own after shutdown/exit. The call has
	// the full request timeout, so bound it with a short goroutine race
	// instead of blocking a caller holding the manager lock.
	sdDone := make(chan struct{})
	go func() {
		_ = c.call("shutdown", nil, nil)
		_ = c.notify("exit", nil)
		close(sdDone)
	}()
	select {
	case <-sdDone:
	case <-time.After(2 * time.Second):
	}
	_ = c.stdin.Close()
	if c.kill != nil {
		c.kill()
	}
	c.markDone()
}

func positionParams(path string, line, character int) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(path)},
		"position":     map[string]any{"line": line, "character": character},
	}
}

// hoverContentsToMarkdown normalizes the LSP hover contents union
// (MarkupContent | MarkedString | MarkedString[]) into one markdown string.
func hoverContentsToMarkdown(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// MarkupContent or MarkedString object form: {kind|language, value}
	var obj struct {
		Value    string `json:"value"`
		Language string `json:"language"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Value != "" {
		if obj.Language != "" {
			return "```" + obj.Language + "\n" + obj.Value + "\n```"
		}
		return obj.Value
	}
	// Bare string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Array of MarkedString
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		parts := make([]string, 0, len(arr))
		for _, el := range arr {
			if part := hoverContentsToMarkdown(el); part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// parseLocations normalizes Location | Location[] | LocationLink[] into a
// flat list.
func parseLocations(raw json.RawMessage) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type lspRange struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
	}
	type lspLocation struct {
		URI       string   `json:"uri"`
		Range     lspRange `json:"range"`
		TargetURI string   `json:"targetUri"`
		TargetSel lspRange `json:"targetSelectionRange"`
	}
	toLocation := func(l lspLocation) (Location, bool) {
		uri, rng := l.URI, l.Range
		if uri == "" {
			uri, rng = l.TargetURI, l.TargetSel
		}
		path := URIToPath(uri)
		if path == "" {
			return Location{}, false
		}
		return Location{Path: path, Line: rng.Start.Line, Character: rng.Start.Character}, true
	}
	var one lspLocation
	if err := json.Unmarshal(raw, &one); err == nil && (one.URI != "" || one.TargetURI != "") {
		if loc, ok := toLocation(one); ok {
			return []Location{loc}
		}
		return nil
	}
	var many []lspLocation
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil
	}
	locs := make([]Location, 0, len(many))
	for _, l := range many {
		if loc, ok := toLocation(l); ok {
			locs = append(locs, loc)
		}
	}
	return locs
}

// PathToURI converts an absolute filesystem path to a file:// URI.
func PathToURI(path string) string {
	path = filepath.ToSlash(path)
	if runtime.GOOS == "windows" && !strings.HasPrefix(path, "/") {
		path = "/" + path // file:///C:/...
	}
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}

// URIToPath converts a file:// URI back to a filesystem path. Returns "" for
// non-file URIs.
func URIToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	path := u.Path
	if runtime.GOOS == "windows" {
		path = strings.TrimPrefix(path, "/")
	}
	return filepath.FromSlash(path)
}
