package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// managerHarness tracks the fake servers a Manager spawned via its start hook.
type managerHarness struct {
	mu      sync.Mutex
	servers []*fakeServer
	spawns  atomic.Int32
	handler func(method string, params json.RawMessage) any
}

func newManagerHarness(t *testing.T, handler func(method string, params json.RawMessage) any) (*Manager, *managerHarness) {
	t.Helper()
	h := &managerHarness{handler: handler}
	m := NewManager(t.TempDir(), "", context.Background())
	m.start = func(_ context.Context, _ string, _ *Language, _, _ map[string]any) (*Client, error) {
		h.spawns.Add(1)
		fs := startFake(h.handler)
		h.mu.Lock()
		h.servers = append(h.servers, fs)
		h.mu.Unlock()
		return fs.client, nil
	}
	t.Cleanup(m.Shutdown)
	return m, h
}

func (h *managerHarness) server(i int) *fakeServer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.servers[i]
}

func hoverOK(method string, _ json.RawMessage) any {
	if method == "textDocument/hover" {
		return map[string]any{"contents": map[string]any{"kind": "markdown", "value": "doc"}}
	}
	return nil
}

func writeGoFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManagerLazyStartAndFileSync(t *testing.T) {
	t.Parallel()

	m, h := newManagerHarness(t, hoverOK)
	if h.spawns.Load() != 0 {
		t.Fatal("manager must not spawn before first request")
	}

	file := writeGoFile(t, m.root, "main.go", "package main\n")
	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if h.spawns.Load() != 1 {
		t.Fatalf("spawns = %d, want 1", h.spawns.Load())
	}

	// Unchanged file: second hover must not re-open or re-send content.
	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover(2): %v", err)
	}
	// Changed file: expect a didChange.
	if err := os.WriteFile(file, []byte("package main\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover(3): %v", err)
	}

	opens, changes := 0, 0
	for _, method := range h.server(0).notificationMethods() {
		switch method {
		case "textDocument/didOpen":
			opens++
		case "textDocument/didChange":
			changes++
		}
	}
	if opens != 1 || changes != 1 {
		t.Errorf("didOpen = %d, didChange = %d; want 1 and 1", opens, changes)
	}
	if h.spawns.Load() != 1 {
		t.Errorf("spawns = %d after three hovers, want 1", h.spawns.Load())
	}
}

func TestManagerIdleShutdownAndRespawn(t *testing.T) {
	t.Parallel()

	m, h := newManagerHarness(t, hoverOK)
	m.idleTimeout = 30 * time.Millisecond
	file := writeGoFile(t, m.root, "main.go", "package main\n")

	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	waitFor(t, "idle shutdown", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.servers) == 0
	})
	// Next request respawns transparently.
	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover after idle: %v", err)
	}
	if h.spawns.Load() != 2 {
		t.Errorf("spawns = %d, want 2 (respawn after idle)", h.spawns.Load())
	}
}

func TestManagerRestartsAfterCrash(t *testing.T) {
	t.Parallel()

	var crashed atomic.Bool
	var h *managerHarness
	handler := func(method string, params json.RawMessage) any {
		if method == "textDocument/hover" && crashed.CompareAndSwap(false, true) {
			// First hover: simulate a gopls crash mid-request.
			h.server(0).kill()
			return nil
		}
		return hoverOK(method, params)
	}
	var m *Manager
	m, h = newManagerHarness(t, handler)
	file := writeGoFile(t, m.root, "main.go", "package main\n")

	got, err := m.Hover(file, 0, 0)
	if err != nil {
		t.Fatalf("Hover should succeed after transparent restart, got: %v", err)
	}
	if got != "doc" {
		t.Errorf("Hover = %q, want %q", got, "doc")
	}
	if h.spawns.Load() != 2 {
		t.Errorf("spawns = %d, want 2 (original + restart)", h.spawns.Load())
	}
}

func TestManagerMissingFile(t *testing.T) {
	t.Parallel()

	m, _ := newManagerHarness(t, hoverOK)
	if _, err := m.Hover(filepath.Join(m.root, "nope.go"), 0, 0); err == nil {
		t.Error("Hover on missing file should error")
	}
}

func TestAvailableDoesNotPanic(t *testing.T) {
	t.Parallel()

	for _, l := range languages {
		_ = l.Available() // smoke: PATH lookup must be side-effect free
	}
}

// TestManagerColdStartDoesNotBlockOtherLanguage pins the per-language
// locking: one language's slow spawn must not head-of-line block requests
// for another language whose server can already answer.
func TestManagerColdStartDoesNotBlockOtherLanguage(t *testing.T) {
	t.Parallel()

	goStarted := make(chan struct{})
	goRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(goRelease) }) }

	h := &managerHarness{handler: hoverOK}
	m := NewManager(t.TempDir(), "", context.Background())
	m.start = func(_ context.Context, _ string, lang *Language, _, _ map[string]any) (*Client, error) {
		if lang.Name == "go" {
			close(goStarted)
			<-goRelease // park the Go cold start
		}
		h.spawns.Add(1)
		fs := startFake(h.handler)
		h.mu.Lock()
		h.servers = append(h.servers, fs)
		h.mu.Unlock()
		return fs.client, nil
	}
	// LIFO: release runs before Shutdown, so a failed test can't deadlock
	// Shutdown on the srv.mu the parked spawn still holds.
	t.Cleanup(m.Shutdown)
	t.Cleanup(release)

	goFile := writeGoFile(t, m.root, "main.go", "package main\n")
	tsFile := writeGoFile(t, m.root, "app.ts", "const x = 1\n")

	goDone := make(chan error, 1)
	go func() {
		_, err := m.Hover(goFile, 0, 0)
		goDone <- err
	}()
	<-goStarted

	// While the Go server is still spawning, a TypeScript hover must complete.
	if _, err := m.Hover(tsFile, 0, 0); err != nil {
		t.Fatalf("ts Hover during go cold start: %v", err)
	}

	release()
	if err := <-goDone; err != nil {
		t.Fatalf("go Hover: %v", err)
	}
}

// startCapture builds a start hook that records the initializationOptions the
// Manager resolved for each spawn.
func startCapture(h *managerHarness, got *map[string]any) startFunc {
	return func(_ context.Context, _ string, _ *Language, initOpts, _ map[string]any) (*Client, error) {
		*got = initOpts
		h.spawns.Add(1)
		fs := startFake(h.handler)
		h.mu.Lock()
		h.servers = append(h.servers, fs)
		h.mu.Unlock()
		return fs.client, nil
	}
}

// A range/PR focus roots the server at a sparse worktree, which holds tracked
// files only — node_modules is not among them. Without the fallback to the
// working tree, typescript-language-server finds no TypeScript there and
// exits during initialize, taking every hover in the focus with it.
func TestManagerResolvesInitOptionsFromDepRootWhenWorktreeLacksDeps(t *testing.T) {
	t.Parallel()

	worktree, depRoot := t.TempDir(), t.TempDir()
	tsserver := writeTSInstall(t, filepath.Join(depRoot, "frontend"))
	file := filepath.Join(worktree, "frontend", "src", "App.tsx")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoFile(t, filepath.Dir(file), "App.tsx", "export const App = () => null;\n")

	h := &managerHarness{handler: hoverOK}
	m := NewManager(worktree, depRoot, context.Background())
	var got map[string]any
	m.start = startCapture(h, &got)
	t.Cleanup(m.Shutdown)

	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	ts, ok := got["tsserver"].(map[string]any)
	if !ok {
		t.Fatalf("initializationOptions = %v, want the working tree's tsserver", got)
	}
	if ts["path"] != tsserver {
		t.Errorf("tsserver.path = %v, want %q", ts["path"], tsserver)
	}
}

// The worktree's own install wins when it has one: depRoot is a fallback for
// what git does not track, never an override of the checkout.
func TestManagerPrefersWorkspaceInstallOverDepRoot(t *testing.T) {
	t.Parallel()

	worktree, depRoot := t.TempDir(), t.TempDir()
	wsTS := writeTSInstall(t, worktree)
	writeTSInstall(t, depRoot)
	file := writeGoFile(t, worktree, "app.ts", "export const x = 1;\n")

	h := &managerHarness{handler: hoverOK}
	m := NewManager(worktree, depRoot, context.Background())
	var got map[string]any
	m.start = startCapture(h, &got)
	t.Cleanup(m.Shutdown)

	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	ts, _ := got["tsserver"].(map[string]any)
	if ts == nil || ts["path"] != wsTS {
		t.Errorf("tsserver.path = %v, want the workspace install %q", ts["path"], wsTS)
	}
}

// No install anywhere: send no initializationOptions at all and leave the
// server's own resolution (a global typescript) in charge.
func TestManagerSendsNoInitOptionsWithoutAnyInstall(t *testing.T) {
	t.Parallel()

	worktree, depRoot := t.TempDir(), t.TempDir()
	file := writeGoFile(t, worktree, "app.ts", "export const x = 1;\n")

	h := &managerHarness{handler: hoverOK}
	m := NewManager(worktree, depRoot, context.Background())
	var got map[string]any
	m.start = startCapture(h, &got)
	t.Cleanup(m.Shutdown)

	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got != nil {
		t.Errorf("initializationOptions = %v, want nil", got)
	}
}

// startCaptureSettings builds a start hook that records the
// workspace/configuration settings the Manager resolved for each spawn.
func startCaptureSettings(h *managerHarness, got *map[string]any) startFunc {
	return func(_ context.Context, _ string, _ *Language, _, settings map[string]any) (*Client, error) {
		*got = settings
		h.spawns.Add(1)
		fs := startFake(h.handler)
		h.mu.Lock()
		h.servers = append(h.servers, fs)
		h.mu.Unlock()
		return fs.client, nil
	}
}

// extraPathsOf pulls python.analysis.extraPaths out of resolved settings.
func extraPathsOf(t *testing.T, settings map[string]any) []string {
	t.Helper()
	py, _ := settings["python"].(map[string]any)
	analysis, _ := py["analysis"].(map[string]any)
	paths, _ := analysis["extraPaths"].([]string)
	return paths
}

// A range/PR focus roots pyright at a sparse worktree with no .venv (it is
// untracked), so without the depRoot fallback every third-party import there
// resolves to Unknown, with no error to say why.
func TestManagerResolvesSettingsFromDepRootWhenWorktreeLacksVenv(t *testing.T) {
	t.Parallel()

	worktree, depRoot := t.TempDir(), t.TempDir()
	sp := writeVenv(t, filepath.Join(depRoot, "services", "api"), ".venv")
	file := filepath.Join(worktree, "services", "api", "app.py")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoFile(t, filepath.Dir(file), "app.py", "import mylib\n")

	h := &managerHarness{handler: hoverOK}
	m := NewManager(worktree, depRoot, context.Background())
	var got map[string]any
	m.start = startCaptureSettings(h, &got)
	t.Cleanup(m.Shutdown)

	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if paths := extraPathsOf(t, got); len(paths) != 1 || paths[0] != sp {
		t.Errorf("extraPaths = %v, want the working tree's venv %q", paths, sp)
	}
}

// The worktree's own venv wins when it has one: depRoot is a fallback for what
// git does not track, never an override of the checkout.
func TestManagerPrefersWorkspaceVenvOverDepRoot(t *testing.T) {
	t.Parallel()

	worktree, depRoot := t.TempDir(), t.TempDir()
	wsSP := writeVenv(t, worktree, ".venv")
	writeVenv(t, depRoot, ".venv")
	file := writeGoFile(t, worktree, "app.py", "import mylib\n")

	h := &managerHarness{handler: hoverOK}
	m := NewManager(worktree, depRoot, context.Background())
	var got map[string]any
	m.start = startCaptureSettings(h, &got)
	t.Cleanup(m.Shutdown)

	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if paths := extraPathsOf(t, got); len(paths) != 1 || paths[0] != wsSP {
		t.Errorf("extraPaths = %v, want the workspace venv %q", paths, wsSP)
	}
}

// No venv anywhere: send no settings at all, which also leaves the capability
// undeclared, and leave pyright's own resolution (an activated venv on PATH, a
// pyrightconfig.json) in charge.
func TestManagerSendsNoSettingsWithoutAnyVenv(t *testing.T) {
	t.Parallel()

	worktree, depRoot := t.TempDir(), t.TempDir()
	file := writeGoFile(t, worktree, "app.py", "import mylib\n")

	h := &managerHarness{handler: hoverOK}
	m := NewManager(worktree, depRoot, context.Background())
	got := map[string]any{"sentinel": true}
	m.start = startCaptureSettings(h, &got)
	t.Cleanup(m.Shutdown)

	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got != nil {
		t.Errorf("settings = %v, want nil", got)
	}
}

// A language without ConfigSettings (Go and TypeScript here) must get nil
// settings whatever sits in the tree: a stray .venv must not change its
// handshake.
func TestManagerSendsNoSettingsToLanguagesWithoutThem(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"main.go", "app.ts"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeVenv(t, root, ".venv")
			file := writeGoFile(t, root, name, "\n")

			h := &managerHarness{handler: hoverOK}
			m := NewManager(root, "", context.Background())
			got := map[string]any{"sentinel": true}
			m.start = startCaptureSettings(h, &got)
			t.Cleanup(m.Shutdown)

			if _, err := m.Hover(file, 0, 0); err != nil {
				t.Fatalf("Hover: %v", err)
			}
			if got != nil {
				t.Errorf("settings for %s = %v, want nil", name, got)
			}
		})
	}
}

func TestManagerDepBase(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		root, depRoot string
		want          string
	}{
		{"working tree focus", "/repo", "", "/repo"},
		{"range focus reads dependencies from the working tree", "/tmp/worktree", "/repo", "/repo"},
	}
	for _, tc := range cases {
		m := NewManager(tc.root, tc.depRoot, context.Background())
		if got := m.depBase(); got != tc.want {
			t.Errorf("%s: depBase() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// pyright reports no progress for its startup analysis and answers correctly
// regardless, so waiting on progress is pure delay. The fake server starts a
// piece of work and never ends it: a Manager that waited would sit out the
// whole warmup budget (15s) before answering.
func TestManagerDoesNotWaitOnProgressForPython(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file := writeGoFile(t, root, "app.py", "x = 1\n")

	m := NewManager(root, "", context.Background())
	m.start = func(_ context.Context, _ string, _ *Language, _, _ map[string]any) (*Client, error) {
		fs := startFake(hoverOK)
		fs.send(map[string]any{
			"jsonrpc": "2.0", "method": "$/progress",
			"params": map[string]any{"token": "t", "value": map[string]any{"kind": "begin", "title": "never ends"}},
		})
		return fs.client, nil
	}
	t.Cleanup(m.Shutdown)

	start := time.Now()
	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Hover took %s, want it to skip the progress wait", took)
	}
}
