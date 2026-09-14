package lsp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultIdleTimeout is how long the manager keeps a language server alive
// after the last request. Multiple crit daemons (e.g. one per worktree) each
// own a manager, so idle shutdown is what keeps N parallel reviews from
// pinning N server processes: only actively-hovered sessions hold one.
const DefaultIdleTimeout = 3 * time.Minute

// startFunc spawns an initialized LSP client for a workspace root and
// language, sending initOpts as initializationOptions. Overridden in tests to
// avoid spawning a real server.
type startFunc func(ctx context.Context, rootDir string, lang *Language, initOpts map[string]any) (*Client, error)

// fileState tracks the sync state of one open document.
type fileState struct {
	version int
	hash    [sha256.Size]byte
}

// serverState is one running language server plus the documents synced to it.
// mu serializes spawn, file sync, and requests for THIS server only —
// per-language, so one language's cold start or warm-up retry loop never
// blocks the other language's requests.
type serverState struct {
	mu     sync.Mutex
	client *Client              // nil until the first request spawns it
	files  map[string]fileState // abs path -> sync state
	// dropped marks a state removed from Manager.servers (idle shutdown);
	// a request that raced the lookup must re-fetch instead of respawning
	// a server into an orphaned, untracked state.
	dropped bool
}

// Manager owns at most one server process per language for a workspace root,
// spawning each on first use and shutting all of them down after idleTimeout
// without requests. All methods are safe for concurrent use; requests are
// serialized per language, which is fine for a single-reviewer localhost
// tool.
type Manager struct {
	root string
	// depRoot is the working tree backing root, set only when root is the
	// sparse worktree of a range/PR focus. That checkout holds tracked files
	// only — node_modules is absent — so a language whose handshake needs a
	// dependency resolves it from depRoot at the same repo-relative path.
	depRoot     string
	baseCtx     context.Context
	idleTimeout time.Duration
	start       startFunc

	mu        sync.Mutex
	servers   map[string]*serverState // Language.Name -> running server
	idleTimer *time.Timer

	rootsMu    sync.Mutex
	extraRoots map[string][]PeekRoot // Language.Name -> resolved ExtraRoots
}

// NewManager creates a manager for the given workspace root. depRoot names
// the working tree backing root and is only meaningful when root is a
// range/PR focus worktree; pass "" when root is the working tree itself.
// baseCtx, when non-nil, bounds the server subprocess lifetimes (daemon
// shutdown kills them). No server is spawned here — only on the first LSP
// request.
func NewManager(root, depRoot string, baseCtx context.Context) *Manager {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return &Manager{
		root:        root,
		depRoot:     depRoot,
		baseCtx:     baseCtx,
		idleTimeout: DefaultIdleTimeout,
		start:       startServer,
		servers:     make(map[string]*serverState),
		extraRoots:  make(map[string][]PeekRoot),
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
// absorbing warm-up errors. Only srv.mu is held across the spawn, the
// request round-trip, and the warm-up retries — never m.mu — so a slow cold
// start for one language cannot head-of-line block the other.
func (m *Manager) withClient(absPath string, fn func(*Client) error) error {
	lang := LanguageForPath(absPath)
	if lang == nil {
		return fmt.Errorf("lsp: no language server registered for %s", absPath)
	}

	srv := m.lockServer(lang)
	defer srv.mu.Unlock()

	deadline := time.Now().Add(warmupTimeout)
	restarted := false
	for {
		if err := m.ensureClient(srv, lang, absPath); err != nil {
			return err
		}
		opened, err := syncFile(srv, lang, absPath)
		if err != nil {
			return err
		}
		if opened {
			// Opening a document is what makes a TypeScript server build the
			// project around it, and it answers from a half-built one without
			// saying so (see WaitReady). Only the didOpen pays this wait: on
			// a warm server the first loop inside WaitReady exits at once.
			srv.client.WaitReady(progressGrace, warmupTimeout)
		}
		reqErr := fn(srv.client)
		if reqErr == nil {
			return nil
		}
		// Restart once when the transport died mid-request (server crash).
		if srv.client.Dead() && !restarted {
			restarted = true
			srv.client.Close()
			srv.client = nil
			srv.files = make(map[string]fileState)
			continue
		}
		// "no views" means gopls's workspace view isn't built yet — transient
		// during startup, so retry briefly instead of surfacing an error.
		if strings.Contains(reqErr.Error(), "no views") && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return reqErr
	}
}

// lockServer returns lang's serverState with srv.mu held, re-fetching when
// an idle shutdown dropped the state between the map lookup and the lock.
func (m *Manager) lockServer(lang *Language) *serverState {
	for {
		srv := m.serverFor(lang)
		srv.mu.Lock()
		if !srv.dropped {
			return srv
		}
		srv.mu.Unlock()
	}
}

// serverFor returns the state tracking lang's server, creating the empty
// state (no process yet — that happens under srv.mu in ensureClient) if
// needed, and re-arms the idle timer.
func (m *Manager) serverFor(lang *Language) *serverState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touchIdleLocked()
	srv, ok := m.servers[lang.Name]
	if !ok {
		srv = &serverState{files: make(map[string]fileState)}
		m.servers[lang.Name] = srv
	}
	return srv
}

// ensureClient spawns + initializes lang's server if srv has no live client.
// absPath is the file the triggering request is about; a language whose
// handshake settings depend on where the file sits in the tree (TypeScript
// picking the nearest node_modules/typescript) resolves them from it.
// Caller holds srv.mu.
func (m *Manager) ensureClient(srv *serverState, lang *Language, absPath string) error {
	if srv.client != nil && !srv.client.Dead() {
		return nil
	}
	if srv.client != nil {
		srv.client.Close()
		srv.client = nil
		srv.files = make(map[string]fileState)
	}
	var initOpts map[string]any
	if lang.InitOptions != nil {
		initOpts = lang.InitOptions(m.root, absPath)
		if initOpts == nil {
			initOpts = m.depInitOptions(lang, absPath)
		}
	}
	client, err := m.start(m.baseCtx, m.root, lang, initOpts)
	if err != nil {
		return err
	}
	srv.client = client
	return nil
}

// depInitOptions retries lang's handshake lookup against depRoot, for the
// file at the same repo-relative path. In range/PR focus the server is rooted
// at a sparse worktree that by design contains tracked files only, so
// TypeScript finds no installation there and the server exits at initialize.
// Borrowing just the dependency location from the working tree keeps the
// server alive while the reviewed sources still come from the checkout.
func (m *Manager) depInitOptions(lang *Language, absPath string) map[string]any {
	if m.depRoot == "" || m.depRoot == m.root {
		return nil
	}
	rel, err := filepath.Rel(m.root, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return lang.InitOptions(m.depRoot, filepath.Join(m.depRoot, rel))
}

// syncFile makes the server's view of absPath match the disk content:
// didOpen on first touch, didChange (full sync) when content changed. Agents
// edit files between review rounds, so disk is always the source of truth.
// It reports whether this call opened the document, which is the point where
// a server starts building the project around it. Caller holds srv.mu.
func syncFile(srv *serverState, lang *Language, absPath string) (opened bool, err error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return false, fmt.Errorf("lsp: reading %s: %w", absPath, err)
	}
	hash := sha256.Sum256(data)
	st, open := srv.files[absPath]
	if open && st.hash == hash {
		return false, nil
	}
	if !open {
		st = fileState{version: 1, hash: hash}
		if err := srv.client.DidOpen(absPath, lang.LanguageID(absPath), string(data), st.version); err != nil {
			return false, err
		}
	} else {
		st.version++
		st.hash = hash
		if err := srv.client.DidChange(absPath, string(data), st.version); err != nil {
			return false, err
		}
	}
	srv.files[absPath] = st
	return !open, nil
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

// dropServerLocked removes lang's server from the map and closes it. Caller
// holds m.mu; the srv.mu acquisition waits for any in-flight request so its
// transport is never yanked mid-call (lock order is always m.mu → srv.mu).
func (m *Manager) dropServerLocked(name string) {
	srv, ok := m.servers[name]
	if !ok {
		return
	}
	delete(m.servers, name)
	srv.mu.Lock()
	srv.dropped = true
	if srv.client != nil {
		srv.client.Close()
		srv.client = nil
	}
	srv.mu.Unlock()
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

// PeekRoots returns the extra source roots (beyond the workspace root) that
// definition/reference peeks may read, resolved per installed language:
// GOROOT and GOMODCACHE for Go, the global node_modules for TypeScript.
// Only a successful lookup is cached — a failure (e.g. the toolchain missing
// from the daemon's PATH) is retried on the next call rather than pinning
// empty roots for the daemon's lifetime.
func (m *Manager) PeekRoots() []PeekRoot {
	m.rootsMu.Lock()
	defer m.rootsMu.Unlock()
	var roots []PeekRoot
	for _, l := range languages {
		if l.ExtraRoots == nil || !l.Available() {
			continue
		}
		cached, ok := m.extraRoots[l.Name]
		if !ok {
			cached = l.ExtraRoots()
			if cached == nil {
				continue
			}
			m.extraRoots[l.Name] = cached
		}
		roots = append(roots, cached...)
	}
	return roots
}

// startServer spawns a real language-server subprocess rooted at rootDir.
func startServer(ctx context.Context, rootDir string, lang *Language, initOpts map[string]any) (*Client, error) {
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
	if err := client.Initialize(rootDir, initOpts); err != nil {
		client.Close()
		return nil, fmt.Errorf("lsp: initializing %s: %w", lang.Command[0], err)
	}
	return client, nil
}
