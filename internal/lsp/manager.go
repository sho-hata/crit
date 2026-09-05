package lsp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultIdleTimeout is how long the manager keeps a language server alive
// after the last request. Multiple crit daemons (e.g. one per worktree) each
// own a manager, so idle shutdown is what keeps N parallel reviews from
// pinning N server processes: only actively-hovered sessions hold one.
const DefaultIdleTimeout = 3 * time.Minute

// GoplsAvailable reports whether gopls is installed on PATH.
func GoplsAvailable() bool {
	for _, l := range languages {
		if l.Name == "go" {
			return l.Available()
		}
	}
	return false
}

// startFunc spawns an initialized LSP client for a workspace root and
// language. Overridden in tests to avoid spawning a real server.
type startFunc func(ctx context.Context, rootDir string, lang *Language) (*Client, error)

// fileState tracks the sync state of one open document.
type fileState struct {
	version int
	hash    [sha256.Size]byte
}

// serverState is one running language server plus the documents synced to it.
type serverState struct {
	client *Client
	files  map[string]fileState // abs path -> sync state
}

// Manager owns at most one server process per language for a workspace root,
// spawning each on first use and shutting all of them down after idleTimeout
// without requests. All methods are safe for concurrent use; requests are
// serialized, which is fine for a single-reviewer localhost tool.
type Manager struct {
	root        string
	baseCtx     context.Context
	idleTimeout time.Duration
	start       startFunc

	mu        sync.Mutex
	servers   map[string]*serverState // Language.Name -> running server
	idleTimer *time.Timer

	goEnvMu    sync.Mutex
	goEnvDone  bool
	goroot     string
	gomodcache string
}

// NewManager creates a manager for the given workspace root. baseCtx, when
// non-nil, bounds the server subprocess lifetimes (daemon shutdown kills
// them). No server is spawned here — only on the first LSP request.
func NewManager(root string, baseCtx context.Context) *Manager {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return &Manager{
		root:        root,
		baseCtx:     baseCtx,
		idleTimeout: DefaultIdleTimeout,
		start:       startServer,
		servers:     make(map[string]*serverState),
	}
}

// Hover returns hover markdown for a 0-based UTF-16 position in absPath.
func (m *Manager) Hover(absPath string, line, character int) (string, error) {
	var out string
	err := m.withClient(absPath, func(c *Client) error {
		var err error
		out, err = c.Hover(absPath, line, character)
		return err
	})
	return out, err
}

// Definition returns definition locations for a 0-based UTF-16 position.
func (m *Manager) Definition(absPath string, line, character int) ([]Location, error) {
	var out []Location
	err := m.withClient(absPath, func(c *Client) error {
		var err error
		out, err = c.Definition(absPath, line, character)
		return err
	})
	return out, err
}

// References returns reference locations.
func (m *Manager) References(absPath string, line, character int) ([]Location, error) {
	var out []Location
	err := m.withClient(absPath, func(c *Client) error {
		var err error
		out, err = c.References(absPath, line, character)
		return err
	})
	return out, err
}

// warmupTimeout bounds the retry window for gopls's "no views" warm-up
// error: right after initialize, requests can arrive before the workspace
// view is built. On a large module this can take a few seconds.
const warmupTimeout = 15 * time.Second

// withClient runs fn against a live, file-synced client for absPath's
// language, restarting the server once if the previous process died and
// absorbing warm-up errors.
func (m *Manager) withClient(absPath string, fn func(*Client) error) error {
	lang := LanguageForPath(absPath)
	if lang == nil {
		return fmt.Errorf("lsp: no language server registered for %s", absPath)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.touchIdleLocked()

	deadline := time.Now().Add(warmupTimeout)
	restarted := false
	for {
		srv, err := m.ensureServerLocked(lang)
		if err != nil {
			return err
		}
		if err := m.syncFileLocked(srv, lang, absPath); err != nil {
			return err
		}
		err = fn(srv.client)
		if err == nil {
			return nil
		}
		// Restart once when the transport died mid-request (server crash).
		if srv.client.Dead() && !restarted {
			restarted = true
			m.dropServerLocked(lang.Name)
			continue
		}
		// "no views" means gopls's workspace view isn't built yet — transient
		// during startup, so retry briefly instead of surfacing an error.
		if strings.Contains(err.Error(), "no views") && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return err
	}
}

// ensureServerLocked spawns + initializes lang's server if not already
// running.
func (m *Manager) ensureServerLocked(lang *Language) (*serverState, error) {
	if srv, ok := m.servers[lang.Name]; ok && !srv.client.Dead() {
		return srv, nil
	}
	m.dropServerLocked(lang.Name)
	client, err := m.start(m.baseCtx, m.root, lang)
	if err != nil {
		return nil, err
	}
	srv := &serverState{client: client, files: make(map[string]fileState)}
	m.servers[lang.Name] = srv
	return srv, nil
}

// syncFileLocked makes the server's view of absPath match the disk content:
// didOpen on first touch, didChange (full sync) when content changed. Agents
// edit files between review rounds, so disk is always the source of truth.
func (m *Manager) syncFileLocked(srv *serverState, lang *Language, absPath string) error {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("lsp: reading %s: %w", absPath, err)
	}
	hash := sha256.Sum256(data)
	st, open := srv.files[absPath]
	if open && st.hash == hash {
		return nil
	}
	if !open {
		st = fileState{version: 1, hash: hash}
		if err := srv.client.DidOpen(absPath, lang.LanguageID(absPath), string(data), st.version); err != nil {
			return err
		}
	} else {
		st.version++
		st.hash = hash
		if err := srv.client.DidChange(absPath, string(data), st.version); err != nil {
			return err
		}
	}
	srv.files[absPath] = st
	return nil
}

// touchIdleLocked (re)arms the idle shutdown timer.
func (m *Manager) touchIdleLocked() {
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(m.idleTimeout, m.idleShutdown)
}

// idleShutdown stops every language server after a quiet period. The next
// request respawns what it needs.
func (m *Manager) idleShutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropAllLocked()
}

func (m *Manager) dropServerLocked(name string) {
	if srv, ok := m.servers[name]; ok {
		srv.client.Close()
		delete(m.servers, name)
	}
}

func (m *Manager) dropAllLocked() {
	for name := range m.servers {
		m.dropServerLocked(name)
	}
}

// Shutdown terminates every running server. Called on daemon shutdown.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.dropAllLocked()
}

// GoEnv returns GOROOT and GOMODCACHE, used to validate that definition peek
// targets stay within known source roots. Only a successful `go env` lookup
// is cached — a failure (e.g. `go` missing from the daemon's PATH) is
// retried on the next call rather than pinning empty roots for the daemon's
// lifetime.
func (m *Manager) GoEnv() (goroot, gomodcache string) {
	m.goEnvMu.Lock()
	defer m.goEnvMu.Unlock()
	if m.goEnvDone {
		return m.goroot, m.gomodcache
	}
	out, err := exec.Command("go", "env", "GOROOT", "GOMODCACHE").Output()
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) >= 1 {
		m.goroot = strings.TrimSpace(lines[0])
	}
	if len(lines) >= 2 {
		m.gomodcache = strings.TrimSpace(lines[1])
	}
	m.goEnvDone = true
	return m.goroot, m.gomodcache
}

// startServer spawns a real language-server subprocess rooted at rootDir.
func startServer(ctx context.Context, rootDir string, lang *Language) (*Client, error) {
	if !lang.Available() {
		return nil, fmt.Errorf("lsp: %s not found on PATH", lang.Command[0])
	}
	cmd := exec.CommandContext(ctx, lang.Command[0], lang.Command[1:]...) //nolint:gosec // argv is fixed in the registry, never user input
	cmd.Dir = rootDir
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: starting %s: %w", lang.Command[0], err)
	}
	// Reap the process when it exits so it never zombies.
	waitDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waitDone) }()
	// kill is invoked by Client.Close after the polite shutdown handshake
	// already got its grace period, so don't wait again — reap or kill now.
	kill := func() {
		select {
		case <-waitDone: // already exited
		default:
			_ = cmd.Process.Kill()
		}
	}
	client := NewClient(stdin, stdout, kill)
	if err := client.Initialize(rootDir); err != nil {
		client.Close()
		return nil, fmt.Errorf("lsp: initializing %s: %w", lang.Command[0], err)
	}
	return client, nil
}
