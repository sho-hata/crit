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

// startFunc spawns an initialized client; tests override it to avoid spawning
// a real server.
type startFunc func(ctx context.Context, rootDir string, lang *Language, initOpts, settings map[string]any) (*Client, error)

type fileState struct {
	version int
	hash    [sha256.Size]byte
}

// serverState is one language server plus the documents synced to it. mu is
// per-language so one language's cold start or warm-up retry loop never blocks
// another language's requests.
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
// spawned on first use and shut down after idleTimeout without requests.
// Requests are serialized per language, which is fine for a single-reviewer
// localhost tool.
type Manager struct {
	root string
	// depRoot is the working tree backing root, set only when root is the
	// sparse worktree of a range/PR focus. That checkout holds tracked files
	// only — node_modules and .venv are absent — so a language whose handshake
	// needs a dependency resolves it from depRoot at the same repo-relative
	// path.
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

// NewManager creates a manager for root. depRoot is the working tree behind a
// range/PR focus worktree, "" when root is the working tree itself. baseCtx
// bounds the server subprocess lifetimes (daemon shutdown kills them).
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

// References returns reference locations, declaration included.
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
// language. Only srv.mu is held across the spawn, the request, and the
// warm-up retries — never m.mu — so a slow cold start for one language cannot
// head-of-line block another.
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
		if opened && !lang.SkipReadyWait {
			// Opening a document is what makes a TypeScript server build the
			// project around it (see WaitReady).
			srv.client.WaitReady(progressGrace, warmupTimeout)
		}
		reqErr := fn(srv.client)
		if reqErr == nil {
			return nil
		}
		if srv.client.Dead() && !restarted {
			restarted = true
			debugf(debugEnabled(), lang.Name, "server died during the request (%v), restarting once", reqErr)
			continue // ensureClient replaces the dead client
		}
		// "no views" means gopls's workspace view isn't built yet — transient
		// during startup, so retry briefly instead of surfacing an error.
		if strings.Contains(reqErr.Error(), "no views") && time.Now().Before(deadline) {
			debugf(debugEnabled(), lang.Name, "workspace not ready (no views), retrying")
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return reqErr
	}
}

// lockServer returns lang's serverState locked, re-fetching when an idle
// shutdown dropped it between the map lookup and the lock.
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

// serverFor creates lang's state without a process (ensureClient spawns it
// under srv.mu) and re-arms the idle timer.
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

// ensureClient spawns lang's server if srv has no live client. absPath is
// passed to the handshake hooks, whose settings depend on where the file sits
// in the tree (nearest node_modules / .venv). Caller holds srv.mu.
func (m *Manager) ensureClient(srv *serverState, lang *Language, absPath string) error {
	if srv.client != nil && !srv.client.Dead() {
		return nil
	}
	if srv.client != nil {
		srv.client.Close()
		srv.client = nil
		srv.files = make(map[string]fileState)
	}
	initOpts := m.handshake(lang.InitOptions, absPath)
	settings := m.handshake(lang.ConfigSettings, absPath)
	client, err := m.start(m.baseCtx, m.root, lang, initOpts, settings)
	if err != nil {
		return err
	}
	srv.client = client
	return nil
}

// handshake runs a handshake hook (InitOptions, ConfigSettings) against the
// workspace root, falling back to the working tree behind it.
func (m *Manager) handshake(hook func(root, absPath string) map[string]any, absPath string) map[string]any {
	if hook == nil {
		return nil
	}
	if v := hook(m.root, absPath); v != nil {
		return v
	}
	return m.depHandshake(hook, absPath)
}

// depHandshake retries a hook against depRoot at the same repo-relative path.
// The range/PR focus worktree holds tracked files only, so node_modules and
// .venv are missing there; borrowing just the dependency location keeps
// third-party code resolvable while the reviewed sources still come from the
// checkout.
func (m *Manager) depHandshake(hook func(root, absPath string) map[string]any, absPath string) map[string]any {
	if m.depRoot == "" || m.depRoot == m.root {
		return nil
	}
	rel, err := filepath.Rel(m.root, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return hook(m.depRoot, filepath.Join(m.depRoot, rel))
}

// syncFile makes the server's view of absPath match the disk, which agents
// edit between review rounds. Caller holds srv.mu.
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

func (m *Manager) touchIdleLocked() {
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(m.idleTimeout, m.idleShutdown)
}

// idleShutdown stops every server; the next request respawns what it needs.
func (m *Manager) idleShutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	debugf(debugEnabled(), "", "idle for %s, shutting down %d server(s)", m.idleTimeout, len(m.servers))
	m.dropAllLocked()
}

// dropServerLocked waits on srv.mu for any in-flight request so its transport
// is never yanked mid-call. Lock order is always m.mu → srv.mu.
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

// Shutdown terminates every running server.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.dropAllLocked()
}

func (m *Manager) depBase() string {
	if m.depRoot != "" {
		return m.depRoot
	}
	return m.root
}

// PeekRoots returns the extra source roots (beyond the workspace root) that
// definition/reference peeks may read: each installed language's ExtraRoots.
// The cache is per Manager (per root), so a project-dependent language never
// sees another workspace's answer. Only a successful lookup is cached: a
// failure (e.g. the toolchain missing from the daemon's PATH) is retried
// rather than pinning empty roots for the daemon's lifetime.
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
			cached = l.ExtraRoots(m.depBase())
			if cached == nil {
				continue
			}
			m.extraRoots[l.Name] = cached
		}
		roots = append(roots, cached...)
	}
	return roots
}

func startServer(ctx context.Context, rootDir string, lang *Language, initOpts, settings map[string]any) (*Client, error) {
	if !lang.Available() {
		return nil, fmt.Errorf("lsp: %s not found on PATH", lang.Command[0])
	}
	cmd := exec.CommandContext(ctx, lang.Command[0], lang.Command[1:]...) //nolint:gosec // argv is fixed in the registry, never user input
	cmd.Dir = rootDir
	debug := debugEnabled()
	if debug {
		cmd.Stderr = &stderrLogger{name: lang.Name}
	} else {
		cmd.Stderr = io.Discard
	}
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
	debugf(debug, lang.Name, "started %s (pid %d) in %s", strings.Join(lang.Command, " "), cmd.Process.Pid, rootDir)
	// Reap the process so it never zombies.
	waitDone := make(chan struct{})
	go func() {
		err := cmd.Wait()
		debugf(debug, lang.Name, "process exited (pid %d): %v", cmd.Process.Pid, err)
		close(waitDone)
	}()
	// Close calls kill after the shutdown handshake's grace period, so don't
	// wait again.
	kill := func() {
		select {
		case <-waitDone: // already exited
		default:
			_ = cmd.Process.Kill()
		}
	}
	client := newClient(lang.Name, stdin, stdout, kill, settings)
	initStart := time.Now()
	if err := client.Initialize(rootDir, initOpts); err != nil {
		client.Close()
		return nil, fmt.Errorf("lsp: initializing %s: %w", lang.Command[0], err)
	}
	debugf(debug, lang.Name, "initialized in %s", roundDuration(time.Since(initStart)))
	return client, nil
}
