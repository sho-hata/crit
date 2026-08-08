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

// defaultIdleTimeout is how long the manager keeps gopls alive after the last
// request. Multiple crit daemons (e.g. one per worktree) each own a manager,
// so idle shutdown is what keeps N parallel reviews from pinning N gopls
// processes: only actively-hovered sessions hold one.
const defaultIdleTimeout = 3 * time.Minute

// goLanguageID is the LSP language identifier sent with didOpen. The manager
// only ever feeds Go files to gopls.
const goLanguageID = "go"

// GoplsAvailable reports whether gopls is installed on PATH.
func GoplsAvailable() bool {
	_, err := exec.LookPath("gopls")
	return err == nil
}

// startFunc spawns an initialized LSP client for a workspace root.
// Overridden in tests to avoid spawning a real gopls.
type startFunc func(ctx context.Context, rootDir string) (*Client, error)

// fileState tracks the sync state of one open document.
type fileState struct {
	version int
	hash    [sha256.Size]byte
}

// Manager owns at most one gopls process for a workspace root, spawning it on
// first use and shutting it down after idleTimeout without requests. All
// methods are safe for concurrent use; requests are serialized, which is fine
// for a single-reviewer localhost tool.
type Manager struct {
	root        string
	baseCtx     context.Context
	idleTimeout time.Duration
	start       startFunc

	mu        sync.Mutex
	client    *Client
	files     map[string]fileState // abs path -> sync state
	idleTimer *time.Timer

	goEnvMu    sync.Mutex
	goEnvDone  bool
	goroot     string
	gomodcache string
}

// NewManager creates a manager for the given workspace root. baseCtx, when
// non-nil, bounds the gopls subprocess lifetime (daemon shutdown kills it).
// gopls is NOT spawned here — only on the first LSP request.
func NewManager(root string, baseCtx context.Context) *Manager {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return &Manager{
		root:        root,
		baseCtx:     baseCtx,
		idleTimeout: defaultIdleTimeout,
		start:       startGopls,
		files:       make(map[string]fileState),
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

// References returns reference locations for a 0-based UTF-16 position.
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

// withClient runs fn against a live, file-synced client, restarting gopls
// once if the previous process died and absorbing warm-up errors.
func (m *Manager) withClient(absPath string, fn func(*Client) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touchIdleLocked()

	deadline := time.Now().Add(warmupTimeout)
	restarted := false
	for {
		if err := m.ensureClientLocked(); err != nil {
			return err
		}
		if err := m.syncFileLocked(absPath); err != nil {
			return err
		}
		err := fn(m.client)
		if err == nil {
			return nil
		}
		// Restart once when the transport died mid-request (gopls crash).
		if m.client.Dead() && !restarted {
			restarted = true
			m.dropClientLocked()
			continue
		}
		// "no views" means the workspace view isn't built yet — transient
		// during startup, so retry briefly instead of surfacing an error.
		if strings.Contains(err.Error(), "no views") && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return err
	}
}

// ensureClientLocked spawns + initializes gopls if not already running.
func (m *Manager) ensureClientLocked() error {
	if m.client != nil && !m.client.Dead() {
		return nil
	}
	m.dropClientLocked()
	client, err := m.start(m.baseCtx, m.root)
	if err != nil {
		return err
	}
	m.client = client
	return nil
}

// syncFileLocked makes the server's view of absPath match the disk content:
// didOpen on first touch, didChange (full sync) when content changed. Agents
// edit files between review rounds, so disk is always the source of truth.
func (m *Manager) syncFileLocked(absPath string) error {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("lsp: reading %s: %w", absPath, err)
	}
	hash := sha256.Sum256(data)
	st, open := m.files[absPath]
	if open && st.hash == hash {
		return nil
	}
	if !open {
		st = fileState{version: 1, hash: hash}
		if err := m.client.DidOpen(absPath, goLanguageID, string(data), st.version); err != nil {
			return err
		}
	} else {
		st.version++
		st.hash = hash
		if err := m.client.DidChange(absPath, string(data), st.version); err != nil {
			return err
		}
	}
	m.files[absPath] = st
	return nil
}

// touchIdleLocked (re)arms the idle shutdown timer.
func (m *Manager) touchIdleLocked() {
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(m.idleTimeout, m.idleShutdown)
}

// idleShutdown stops gopls after a quiet period. The next request respawns it.
func (m *Manager) idleShutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropClientLocked()
}

func (m *Manager) dropClientLocked() {
	if m.client != nil {
		m.client.Close()
		m.client = nil
	}
	m.files = make(map[string]fileState)
}

// Shutdown terminates gopls if running. Called on daemon shutdown.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.dropClientLocked()
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

// startGopls spawns a real gopls subprocess rooted at rootDir.
func startGopls(ctx context.Context, rootDir string) (*Client, error) {
	if !GoplsAvailable() {
		return nil, fmt.Errorf("lsp: gopls not found on PATH")
	}
	cmd := exec.CommandContext(ctx, "gopls")
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
		return nil, fmt.Errorf("lsp: starting gopls: %w", err)
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
		return nil, fmt.Errorf("lsp: initializing gopls: %w", err)
	}
	return client, nil
}
