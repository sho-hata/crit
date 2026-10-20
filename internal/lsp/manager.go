package lsp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sho-hata/crit/internal/sandbox"
)

// DefaultIdleTimeout is how long the manager keeps a language server alive
// after the last request. Multiple crit daemons (e.g. one per worktree) each
// own a manager, so idle shutdown is what keeps N parallel reviews from
// pinning N server processes: only actively-hovered sessions hold one.
const DefaultIdleTimeout = 3 * time.Minute

// startFunc spawns an initialized client; tests override it to avoid spawning
// a real server.
type startFunc func(ctx context.Context, lang *Language, initOpts, settings map[string]any) (*Client, error)

type fileState struct {
	version int
	hash    [sha256.Size]byte
	// modTime and size, from a stat of the file that was read, let
	// syncSessionFiles skip re-reading an untouched file.
	modTime time.Time
	size    int64
}

// serverState is one language server plus the documents synced to it. mu is
// per-language so one language's cold start or warm-up retry loop never blocks
// another language's requests.
type serverState struct {
	mu     sync.Mutex
	client *Client              // nil until the first request spawns it
	files  map[string]fileState // abs path -> sync state
	// reviewDocs marks the open documents syncSessionFiles keeps open for
	// the review, so it can close one the review no longer lists (deleted,
	// renamed away, or declined by Language.OpenSessionFile).
	reviewDocs map[string]bool
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
	depRoot string
	// jail is the only way a server or helper process gets started. jailErr
	// is Open's failure, surfaced on every spawn rather than worked around.
	jail        *sandbox.Jail
	jailErr     error
	baseCtx     context.Context
	idleTimeout time.Duration
	start       startFunc

	mu        sync.Mutex
	servers   map[string]*serverState // Language.Name -> running server
	idleTimer *time.Timer

	rootsMu    sync.Mutex
	extraRoots map[string][]PeekRoot // Language.Name -> resolved ExtraRoots

	// sessionFiles lists the review's files, relative to root; nil opens
	// only the requested file. See syncSessionFiles.
	sessionFiles func() []string
}

// maxSessionDocuments caps how many review files syncSessionFiles keeps open
// per language, so a review of a huge generated change doesn't push every
// file through the server on each request.
const maxSessionDocuments = 100

// NewManager creates a manager for root. depRoot is the working tree behind a
// range/PR focus worktree, "" when root is the working tree itself.
// scratchBase is where the servers may write (one directory per language
// under it). baseCtx bounds the server subprocess lifetimes (daemon shutdown
// kills them).
func NewManager(root, depRoot, scratchBase string, baseCtx context.Context) *Manager {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	jail, err := sandbox.Open(sandbox.Trees{Root: root, DepRoot: depRoot}, scratchBase)
	m := &Manager{
		root:        root,
		depRoot:     depRoot,
		jail:        jail,
		jailErr:     err,
		baseCtx:     baseCtx,
		idleTimeout: DefaultIdleTimeout,
		servers:     make(map[string]*serverState),
		extraRoots:  make(map[string][]PeekRoot),
	}
	m.start = m.startServer
	return m
}

// SetSessionFiles makes every request also open the review's files of its
// language. files returns paths relative to root and is called per request,
// so it always reflects the current file list. Call before the first request.
func (m *Manager) SetSessionFiles(files func() []string) {
	m.sessionFiles = files
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

// contentModifiedCode is LSP's ContentModified: the server's state changed
// under the request, so it dropped an answer that would have been stale. The
// spec leaves the retry to the client; contentModifiedBackoff bounds it to a
// couple of quick tries.
const contentModifiedCode = -32801

var contentModifiedBackoff = []time.Duration{40 * time.Millisecond, 120 * time.Millisecond}

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
	modified := 0
	for {
		if err := m.ensureClient(srv, lang, absPath); err != nil {
			return err
		}
		opened, err := m.syncDocuments(srv, lang, absPath)
		if err != nil {
			return err
		}
		if opened && !lang.SkipReadyWait {
			// Opening a document is what makes a TypeScript server build the
			// project around it (see WaitReady).
			srv.client.WaitReady(m.readyGrace(lang, absPath), warmupTimeout)
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
		var respErr *ResponseError
		if lang.NoResult != nil && errors.As(reqErr, &respErr) && lang.NoResult(respErr) {
			debugf(debugEnabled(), lang.Name, "nothing at this position: %v", reqErr)
			return nil
		}
		if wait, ok := retryDelay(lang, reqErr, &modified, deadline); ok {
			time.Sleep(wait)
			continue // syncFile re-reads the document before the retry
		}
		return reqErr
	}
}

// startupProgressGrace bounds the wait for progress a server is known to
// report (Language.StartupProgress). It only runs out if that report never
// comes; on a loaded machine it can begin over a second after the didOpen.
const startupProgressGrace = 10 * time.Second

// readyGrace is how long WaitReady waits for lang's server to report work
// after absPath is opened.
func (m *Manager) readyGrace(lang *Language, absPath string) time.Duration {
	if lang.StartupProgress != nil && lang.StartupProgress(m.root, absPath) {
		return startupProgressGrace
	}
	return progressGrace
}

// retryDelay reports whether withClient should ask again after reqErr, and
// how long to wait first. modified counts the ContentModified retries spent.
func retryDelay(lang *Language, reqErr error, modified *int, deadline time.Time) (time.Duration, bool) {
	var respErr *ResponseError
	if errors.As(reqErr, &respErr) && respErr.Code == contentModifiedCode && *modified < len(contentModifiedBackoff) {
		debugf(debugEnabled(), lang.Name, "content modified during the request, retrying")
		wait := contentModifiedBackoff[*modified]
		*modified++
		return wait, true
	}
	// "no views" means gopls's workspace view isn't built yet — transient
	// during startup, so retry briefly instead of surfacing an error.
	if strings.Contains(reqErr.Error(), "no views") && time.Now().Before(deadline) {
		debugf(debugEnabled(), lang.Name, "workspace not ready (no views), retrying")
		return 200 * time.Millisecond, true
	}
	return 0, false
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
		srv = &serverState{files: make(map[string]fileState), reviewDocs: make(map[string]bool)}
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
		srv.reviewDocs = make(map[string]bool)
	}
	if m.jailErr != nil {
		return fmt.Errorf("lsp: %w", m.jailErr)
	}
	// The scratch is named before the spawn so an InitOptions hook can point
	// the server's own writes at it; Command creates it.
	scratch := m.jail.Workspace(lang.Name).Scratch
	var initOpts map[string]any
	if lang.InitOptions != nil {
		initOpts = m.handshake(func(root, absPath string) map[string]any {
			return lang.InitOptions(Workspace{Root: root, Scratch: scratch}, absPath)
		}, absPath)
	}
	settings := m.handshake(lang.ConfigSettings, absPath)
	client, err := m.start(m.baseCtx, lang, initOpts, settings)
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
	data, fi, err := readFileStat(absPath)
	if err != nil {
		return false, fmt.Errorf("lsp: reading %s: %w", absPath, err)
	}
	hash := sha256.Sum256(data)
	st, open := srv.files[absPath]
	st.modTime, st.size = fi.ModTime(), fi.Size()
	if open && st.hash == hash {
		srv.files[absPath] = st
		return false, nil
	}
	if !open {
		st.version, st.hash = 1, hash
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

// syncDocuments syncs the requested file, then the review's other files of
// lang. Only the requested file's failure is an error. Caller holds srv.mu.
func (m *Manager) syncDocuments(srv *serverState, lang *Language, absPath string) (opened bool, err error) {
	opened, err = syncFile(srv, lang, absPath)
	if err != nil {
		return false, err
	}
	return m.syncSessionFiles(srv, lang, absPath) || opened, nil
}

// readFileStat reads absPath along with the stat of the same open file.
func readFileStat(absPath string) ([]byte, os.FileInfo, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	return data, fi, err
}

// syncSessionFiles opens the review's other files of lang that
// Language.OpenSessionFile accepts and keeps them in sync with the disk. A
// document it opened that the review no longer lists, or that is gone from
// disk, is closed so the server stops answering from its last content.
// Reports whether anything was newly opened. Caller holds srv.mu.
func (m *Manager) syncSessionFiles(srv *serverState, lang *Language, requested string) (opened bool) {
	docs := m.sessionDocuments(lang, requested)
	listed := make(map[string]bool, len(docs))
	for _, absPath := range docs {
		listed[absPath] = true
		if unchangedOnDisk(srv, absPath) {
			srv.reviewDocs[absPath] = true
			continue
		}
		newlyOpened, err := syncFile(srv, lang, absPath)
		if err != nil {
			listed[absPath] = false // deleted: close it below
			continue
		}
		srv.reviewDocs[absPath] = true
		opened = opened || newlyOpened
	}
	for absPath := range srv.reviewDocs {
		if listed[absPath] || absPath == requested {
			continue
		}
		delete(srv.reviewDocs, absPath)
		if _, open := srv.files[absPath]; open {
			delete(srv.files, absPath)
			_ = srv.client.DidClose(absPath)
		}
	}
	return opened
}

// sessionDocuments lists the review files of lang to keep open besides
// requested, at most maxSessionDocuments. OpenSessionFile is asked on every
// request: its answer depends on files a review round can add or remove.
func (m *Manager) sessionDocuments(lang *Language, requested string) []string {
	if m.sessionFiles == nil || lang.OpenSessionFile == nil {
		return nil
	}
	var docs []string
	for _, rel := range m.sessionFiles() {
		if l := LanguageForPath(rel); l == nil || l.Name != lang.Name {
			continue
		}
		absPath := filepath.Join(m.root, rel)
		if absPath == requested || !lang.OpenSessionFile(m.root, absPath) {
			continue
		}
		if docs = append(docs, absPath); len(docs) == maxSessionDocuments {
			break
		}
	}
	return docs
}

// unchangedOnDisk reports whether absPath is open and its stat still matches
// the one taken at the last sync. Caller holds srv.mu.
func unchangedOnDisk(srv *serverState, absPath string) bool {
	st, open := srv.files[absPath]
	if !open {
		return false
	}
	fi, err := os.Stat(absPath)
	return err == nil && fi.Size() == st.size && fi.ModTime().Equal(st.modTime)
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
			cached = l.ExtraRoots(m.depBase(), m.run(l))
			if cached == nil {
				continue
			}
			m.extraRoots[l.Name] = cached
		}
		roots = append(roots, cached...)
	}
	return roots
}

// run confines a language's helper commands (go env, npm root -g, python3)
// like its server, with cwd in its scratch rather than the tree.
func (m *Manager) run(lang *Language) Run {
	return func(argv ...string) ([]byte, error) {
		if m.jailErr != nil {
			return nil, m.jailErr
		}
		return m.jail.Output(m.baseCtx, lang.Name, func(Workspace) sandbox.Process {
			return sandbox.Process{Argv: argv}
		})
	}
}

// confinement describes, for the debug log and errors, what the server
// runs under.
func (m *Manager) confinement(scratch string) string {
	b := sandbox.Detect()
	if b.Name == "" {
		return "unconfined (" + b.Reason + ")"
	}
	trees := []string{m.root}
	if m.depRoot != "" && m.depRoot != m.root {
		trees = append(trees, m.depRoot)
	}
	return fmt.Sprintf("confined by %s: no network, writes only under %s, no exec under %s or the scratch, secrets unreadable",
		b.Name, scratch, strings.Join(trees, " and "))
}

// exitGrace is how long a server may take to exit after "exit" before it
// is killed; the servers crit ships leave within milliseconds.
const exitGrace = 2 * time.Second

// supervise watches the server from the moment it starts and returns the
// kill that Close calls once it has sent "exit". The server gets exitGrace
// to leave on its own, because that is when node writes its compile cache
// and intelephense its index; a kill straight after "exit" threw both away
// on every shutdown, so every start was cold. Exit is observed without
// reaping: a zombie leader keeps the group id reserved until the one
// cleanup goroutine has killed the group. Monitoring starts at once, since
// children may keep stdout open after the leader crashes.
func supervise(cmd *exec.Cmd, name string, debug bool) func() {
	exit := make(chan error, 1)
	go func() { exit <- waitProcessExit(cmd) }()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		var monitorErr error
		select {
		case monitorErr = <-exit:
			killTree(cmd)
		case <-stop:
			killTree(cmd)
			// Finish monitoring before Wait can release the PID, even if
			// the monitor goroutine was slow to start.
			monitorErr = <-exit
		}
		if monitorErr != nil {
			debugf(debug, name, "monitoring process (pid %d): %v", cmd.Process.Pid, monitorErr)
		}
		err := cmd.Wait()
		debugf(debug, name, "process exited (pid %d): %v; denials: %s", cmd.Process.Pid, err, sandbox.DenialHint)
		close(done)
	}()
	var stopOnce sync.Once
	return func() {
		timer := time.NewTimer(exitGrace)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
			debugf(debug, name, "process (pid %d) ignored exit, killing", cmd.Process.Pid)
			stopOnce.Do(func() { close(stop) })
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			debugf(debug, name, "process (pid %d) still running after the kill", cmd.Process.Pid)
		}
	}
}

func (m *Manager) startServer(ctx context.Context, lang *Language, initOpts, settings map[string]any) (*Client, error) {
	if !lang.Available() {
		return nil, fmt.Errorf("lsp: %s not found on PATH", lang.Command[0])
	}
	debug := debugEnabled()
	ws := m.jail.Workspace(lang.Name)
	cmd, err := m.jail.Command(ctx, lang.Name, func(Workspace) sandbox.Process {
		p := sandbox.Process{Argv: slices.Clone(lang.Command), Stderr: io.Discard}
		if lang.Args != nil {
			p.Argv = append(p.Argv, lang.Args(ws)...)
		}
		if lang.Env != nil {
			p.Env = lang.Env(ws)
		}
		if debug {
			p.Stderr = &stderrLogger{name: lang.Name}
		}
		return p
	})
	if err != nil {
		return nil, fmt.Errorf("lsp: %s: %w", lang.Command[0], err)
	}
	isolate(cmd)
	// Wait would otherwise block on a stderr pipe a forked child still holds.
	cmd.WaitDelay = exitGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// The daemon's context ends before ShutdownLSP runs, so cancellation
	// asks the server to leave (stdin EOF) rather than killing it; the
	// monitor below and WaitDelay still bound a server that stays.
	cmd.Cancel = stdin.Close
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: starting %s: %w", lang.Command[0], err)
	}
	under := m.confinement(ws.Scratch)
	debugf(debug, lang.Name, "started %s (pid %d) in %s, %s", strings.Join(lang.Command, " "), cmd.Process.Pid, m.root, under)
	kill := supervise(cmd, lang.Name, debug)
	client := newClient(lang.Name, stdin, stdout, kill, settings)
	initStart := time.Now()
	if err := client.Initialize(m.root, initOpts); err != nil {
		client.Close()
		return nil, fmt.Errorf("lsp: initializing %s (%s): %w", lang.Command[0], under, err)
	}
	debugf(debug, lang.Name, "initialized in %s", roundDuration(time.Since(initStart)))
	return client, nil
}
