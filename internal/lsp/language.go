package lsp

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// PeekRoot is one directory tree outside the workspace root that definition
// and reference peeks are allowed to read, with the display prefix the UI
// shows for paths under it.
type PeekRoot struct {
	Path  string // absolute directory
	Label string // display prefix, e.g. "$GOROOT"
	// LocalEnv marks a root holding packages installed in the reviewer's own
	// environment (e.g. a virtualenv's site-packages). Under range/PR focus
	// these can differ from the dependencies of the SHA being reviewed.
	LocalEnv bool
}

// Language describes one language-server integration: which files it covers,
// how to spawn the server, and what a range-focus sparse worktree must
// contain for the server to build a workspace view.
type Language struct {
	// Name is the stable registry key (e.g. "go", "typescript").
	Name string
	// Command is the server argv; Command[0] is looked up on PATH. Only the
	// fixed binary name is ever spawned — there is deliberately no config
	// key for the path, so a malicious repo cannot hijack the command.
	Command []string
	// Env lists KEY=VALUE pairs set for the server process on top of the
	// allowlisted environment (see serverEnv). Fixed per language, never
	// read from config or the reviewed tree.
	Env []string
	// Requires reports why the language cannot run even though its binary is
	// on PATH — a dependency crit has to supply from outside the reviewed
	// tree (TypeScript's tsserver) — or nil. root is the workspace, "" when
	// only general availability is asked (Available). Checked before each
	// spawn and cached with Available.
	Requires func(root string) error
	// IDByExt maps a file extension (lower-case, no dot) to the LSP
	// languageId sent with didOpen.
	IDByExt map[string]string
	// SparsePatterns is what the range-focus LSP worktree checks out: the
	// source and project files the server needs, not the rest of the tree.
	SparsePatterns []string
	// InitOptions returns the initializationOptions for a server rooted at
	// root whose first request is about absPath, or nil. Runs on the request
	// path (once per server spawn) — keep it filesystem-cheap.
	InitOptions func(root, absPath string) map[string]any
	// ConfigSettings answers workspace/configuration pulls, keyed by section
	// name ("python"). Some servers take settings only this way (pyright
	// ignores initializationOptions), hence separate from InitOptions. A nil
	// result leaves the capability undeclared. Same cost rule as InitOptions,
	// and never execute anything the repo supplies.
	ConfigSettings func(root, absPath string) map[string]any
	// LocalEnv says third-party answers (types, definitions) for this
	// language come from the reviewer's own environment. Under range/PR focus
	// the reviewed SHA's dependencies are not part of git, so they resolve
	// against whatever is installed locally: a bumped version shows the old
	// API, a newly added dependency shows Unknown. The UI notes this so the
	// answers are not mistaken for the PR's.
	LocalEnv bool
	// SkipReadyWait skips the wait for startup progress after a document is
	// opened (see Client.WaitReady). Set it for a server that queues requests
	// until its analysis is done and reports no progress for it (pyright): the
	// wait would only sit out its grace period. Don't set it for a server that
	// answers from a half-built project.
	SkipReadyWait bool
	// OpenSessionFile reports whether the Manager should open absPath, a
	// review file of this language, next to each request
	// (Manager.SetSessionFiles); nil opens none. It is for a server that only
	// searches loaded files, where references from a changed file nobody
	// hovered are otherwise missing. Opening costs the first request the time
	// to load the files, so leave it nil for a server that indexes the
	// workspace anyway (gopls). Same cost rule as InitOptions.
	OpenSessionFile func(root, absPath string) bool
	// StartupProgress reports whether the server is known to report
	// work-done progress for loading absPath's project, so the first wait
	// after opening it gives the report startupProgressGrace, not
	// progressGrace, to begin. On a busy machine starting the server alone can
	// outlast progressGrace, and an answer before the project loads leaves out
	// what it alone holds. nil for a server that may report nothing. Same cost
	// rule as InitOptions.
	StartupProgress func(root, absPath string) bool
	// NoResult reports whether an error answer to a position request
	// (hover, definition, references) means there is simply nothing at that
	// position, for a server that says so with an error instead of null; nil
	// for one that answers null like gopls. The Manager turns such an error
	// into an empty result: the UI counts errors toward its failure breaker,
	// and hovering over whitespace must not trip it.
	NoResult func(*ResponseError) bool
	// ExtraRoots resolves the language's out-of-workspace source roots where
	// definitions can land (e.g. GOROOT for Go, the global node_modules for
	// TypeScript). root is the tree the language's dependencies live in — the
	// workspace root, or the working tree backing it under range/PR focus —
	// for languages whose roots depend on the project rather than the
	// machine. May run external commands; the Manager caches successful
	// results per root. A nil result means the lookup failed and may be
	// retried.
	ExtraRoots func(root string) []PeekRoot
	// CacheSweep is housekeeping for a server's on-disk cache that the server
	// would do itself if crit's idle shutdown didn't stop it first. Run once
	// per crit process in the background after the server first spawns, with
	// the server's working directory; nil for none.
	CacheSweep func(root string)

	sweepOnce sync.Once
}

// sweepCacheOnce starts CacheSweep in the background the first time it is
// called in this process.
func (l *Language) sweepCacheOnce(root string) {
	if l.CacheSweep != nil {
		l.sweepOnce.Do(func() { go l.CacheSweep(root) })
	}
}

// node_modules, .venv, .terraform and vendor are untracked, so a range-focus
// sparse worktree has none of them. Manager.depRoot borrows the working tree's
// for the handshake (the site-packages for pyright, the Composer vendor dir
// for intelephense); TypeScript cross-package hover/definition and Terraform
// registry/git module references still degrade there, intra-repo symbols keep
// working.
//
// Nothing here runs code the reviewed tree supplies: the binaries come from
// PATH, tsserver from outside the tree (trustedTSServer), terraform from
// os.DevNull, and a server never gets an interpreter path (python.pythonPath)
// it would execute. Keep it that way — a PR under review is untrusted input.
var languages = []*Language{
	{
		Name:    "go",
		Command: []string{"gopls"},
		// The go command gopls drives honours a `toolchain` line in go.mod by
		// downloading and running that Go release (GOTOOLCHAIN=auto, the
		// default). The download is sumdb-checked, but a review should not
		// install toolchains on the reviewer's machine; local means "use the
		// one on PATH" and errors on a module that needs a newer one.
		Env:     []string{"GOTOOLCHAIN=local"},
		IDByExt: map[string]string{"go": "go"},
		SparsePatterns: []string{
			"*.go", "go.mod", "go.sum", "go.work", "go.work.sum",
		},
		ExtraRoots: goExtraRoots,
		CacheSweep: sweepGoplsCache,
	},
	{
		Name:            "typescript",
		Command:         []string{"typescript-language-server", "--stdio"},
		OpenSessionFile: tsOpenSessionFile,
		StartupProgress: tsStartupProgress,
		IDByExt: map[string]string{
			"ts":  "typescript",
			"mts": "typescript",
			"cts": "typescript",
			"tsx": "typescriptreact",
			"js":  "javascript",
			"mjs": "javascript",
			"cjs": "javascript",
			"jsx": "javascriptreact",
		},
		SparsePatterns: []string{
			"*.ts", "*.mts", "*.cts", "*.tsx",
			"*.js", "*.mjs", "*.cjs", "*.jsx",
			"package.json", "tsconfig*.json", "jsconfig.json",
		},
		InitOptions: tsInitOptions,
		Requires:    tsRequires,
		ExtraRoots:  tsExtraRoots,
	},
	{
		Name:    "python",
		Command: []string{"pyright-langserver", "--stdio"},
		IDByExt: map[string]string{"py": "python", "pyi": "python"},
		SparsePatterns: []string{
			"*.py", "*.pyi", "pyproject.toml", "pyrightconfig.json",
		},
		ConfigSettings:  pyConfigSettings,
		LocalEnv:        true,
		SkipReadyWait:   true,
		OpenSessionFile: pyOpenSessionFile,
		ExtraRoots:      pyExtraRoots,
	},
	{
		Name:           "terraform",
		Command:        []string{"terraform-ls", "serve"},
		IDByExt:        map[string]string{"tf": "terraform", "tfvars": "terraform-vars"},
		SparsePatterns: []string{"*.tf", "*.tfvars"},
		InitOptions:    tfInitOptions,
		NoResult:       tfNoResult,
		SkipReadyWait:  true,
	},
	{
		Name:    "php",
		Command: []string{"intelephense", "--stdio"},
		IDByExt: map[string]string{"php": "php", "phtml": "php"},
		SparsePatterns: []string{
			"*.php", "*.phtml", "composer.json", "composer.lock",
		},
		ConfigSettings: phpConfigSettings,
		LocalEnv:       true,
		ExtraRoots:     phpExtraRoots,
	},
}

// tsInitOptions pins tsserver to an install outside the reviewed tree and
// turns off automatic typing acquisition.
//
// tsserver.js is a Node program. Without tsserver.path,
// typescript-language-server prefers the workspace's own
// node_modules/typescript (and .yarn/sdks, .pnpm/sdks) over its fallback and
// bundled copies, so it would run whatever JavaScript the checkout — a PR
// under review — ships, with the reviewer's privileges (the shape of
// CVE-2026-49241, where an editor extension honoured a repo-supplied
// typescript.tsdk). An invalid path does not help: the server falls through
// to the workspace copy. So the path always names a trusted install, and
// tsRequires keeps the server from starting when there is none. The tree's
// node_modules remains what it is to tsserver anyway: the .d.ts files types
// resolve from.
//
// Typing acquisition would fetch @types packages from npm for untyped imports
// and write them under the reviewer's cache; a review is not the time for
// either.
func tsInitOptions(root, _ string) map[string]any {
	tsserver := trustedTSServerFor(root)
	if tsserver == "" {
		return nil
	}
	return map[string]any{
		"tsserver":                          map[string]any{"path": tsserver},
		"disableAutomaticTypingAcquisition": true,
	}
}

// tsRequires is Language.Requires for TypeScript: the server must not start
// without a tsserver crit chose (see tsInitOptions).
func tsRequires(root string) error {
	if trustedTSServerFor(root) == "" {
		return errors.New("lsp: no typescript install outside the reviewed tree " +
			"(install it next to typescript-language-server, e.g. npm install -g typescript)")
	}
	return nil
}

// trustedTSServerFor is trustedTSServer, rejecting a result inside root: a
// PATH that includes the tree's own node_modules/.bin (direnv, a project
// shell) must not turn the tree's typescript into a trusted one.
func trustedTSServerFor(root string) string {
	tsserver := trustedTSServer()
	if tsserver == "" || (root != "" && within(root, tsserver)) {
		return ""
	}
	return tsserver
}

// within reports whether path lies under dir (or is dir), comparing resolved
// paths so a symlinked prefix (macOS's /var -> /private/var) cannot hide the
// relation.
func within(dir, path string) bool {
	dir, path = resolved(dir), resolved(path)
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolved is filepath.EvalSymlinks with the cleaned input as fallback.
func resolved(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// tsServerCache maps PATH to a tsServerEntry, with the TTL of availableCache:
// a typescript installed mid-session shows up within it.
var tsServerCache sync.Map

type tsServerEntry struct {
	path string
	at   time.Time
}

// trustedTSServer returns the tsserver.js typescript-language-server may
// run, or "": a typescript package installed alongside the server, or the
// one behind tsc on PATH, or the global npm root — never one found by walking
// the reviewed tree. The order is cheapest first; only the last spawns npm.
// Result cached per PATH for availableTTL.
func trustedTSServer() string {
	return trustedTSServerAt(time.Now())
}

func trustedTSServerAt(now time.Time) string {
	key := os.Getenv("PATH")
	if v, ok := tsServerCache.Load(key); ok {
		if e := v.(tsServerEntry); now.Sub(e.at) < availableTTL {
			return e.path
		}
	}
	path := findTrustedTSServer()
	tsServerCache.Store(key, tsServerEntry{path: path, at: now})
	return path
}

func findTrustedTSServer() string {
	// 1. Next to typescript-language-server: its own node_modules (a nested
	//    dependency) or the node_modules it was installed into (a sibling
	//    global install). Symlinks are resolved because a global bin is a
	//    link into lib/node_modules.
	for _, bin := range []string{"typescript-language-server", "tsc"} {
		exe, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		if tsserver := tsServerNear(exe); tsserver != "" {
			return tsserver
		}
	}
	// 2. The global npm root, for an npm prefix whose bin dir is not on PATH.
	if dir := npmGlobalRoot(); dir != "" {
		if tsserver := tsServerIn(dir); tsserver != "" {
			return tsserver
		}
	}
	return ""
}

// tsServerNear looks for node_modules/typescript/lib/tsserver.js in the
// directories above exe, up to and including the first node_modules
// (exclusive of anything beyond it, which would be an unrelated tree).
func tsServerNear(exe string) string {
	dir := filepath.Dir(exe)
	for {
		if filepath.Base(dir) == "node_modules" {
			return tsServerIn(dir)
		}
		if tsserver := tsServerIn(filepath.Join(dir, "node_modules")); tsserver != "" {
			return tsserver
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// tsServerIn returns nodeModules/typescript/lib/tsserver.js if it is a file.
func tsServerIn(nodeModules string) string {
	candidate := filepath.Join(nodeModules, "typescript", "lib", "tsserver.js")
	// Stat, not Lstat: pnpm links the package, and the link target is the
	// real install.
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return candidate
	}
	return ""
}

// tsOpenSessionFile reports whether absPath may fall outside every
// configured project within root. Such a file belongs to an inferred project,
// which tsserver builds from the open files and their imports only, so
// references from it are found only once it is open. A configured project
// already covers its files, and opening them would only add the cost of
// checking them. A tsconfig.json takes JavaScript only with allowJs, which
// may come through extends, so a JavaScript file is left out only under a
// jsconfig.json (allowJs by definition). A config whose include or files
// leave absPath out still counts as covering it.
func tsOpenSessionFile(root, absPath string) bool {
	configs := tsConfigNames
	if tsJavaScriptExt[strings.ToLower(strings.TrimPrefix(filepath.Ext(absPath), "."))] {
		configs = []string{"jsconfig.json"}
	}
	return !tsConfigAbove(root, absPath, configs)
}

// tsStartupProgress: typescript-language-server reports "Initializing JS/TS
// language features" while it loads a configured project, and nothing for an
// inferred one.
func tsStartupProgress(root, absPath string) bool {
	return tsConfigAbove(root, absPath, tsConfigNames)
}

var tsConfigNames = []string{"tsconfig.json", "jsconfig.json"}

// tsConfigAbove reports whether one of names is a file in absPath's
// directory or a parent of it within root.
func tsConfigAbove(root, absPath string, names []string) bool {
	return walkUpWithin(root, absPath, func(dir string) bool {
		for _, name := range names {
			if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
				return true
			}
		}
		return false
	})
}

var tsJavaScriptExt = map[string]bool{"js": true, "mjs": true, "cjs": true, "jsx": true}

// walkUpWithin calls found for absPath's directory and each parent through
// root (inclusive), stopping at the first true, and reports whether one was.
// It never leaves root: a file outside it is not ours to resolve
// dependencies for.
func walkUpWithin(root, absPath string, found func(dir string) bool) bool {
	root = filepath.Clean(root)
	dir := filepath.Dir(absPath)
	if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	for {
		if found(dir) {
			return true
		}
		if dir == root {
			return false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// goExtraRoots ignores root: GOROOT and GOMODCACHE are machine-wide.
func goExtraRoots(_ string) []PeekRoot {
	out, err := exec.Command("go", "env", "GOROOT", "GOMODCACHE").Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	labels := []string{"$GOROOT", "$GOMODCACHE"}
	var roots []PeekRoot
	for i, label := range labels {
		if i >= len(lines) {
			break
		}
		if dir := strings.TrimSpace(lines[i]); dir != "" {
			roots = append(roots, PeekRoot{Path: dir, Label: label})
		}
	}
	return roots
}

// npmGlobalRoot is `npm root -g`, or "" when npm is missing or fails.
func npmGlobalRoot() string {
	out, err := exec.Command("npm", "root", "-g").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// tsExtraRoots is where lib.*.d.ts definitions land: the typescript package
// tsserver runs from (trustedTSServer) and the global npm root, the two
// places a trusted install lives. Machine-wide, so root is ignored.
func tsExtraRoots(_ string) []PeekRoot {
	var roots []PeekRoot
	if tsserver := trustedTSServer(); tsserver != "" {
		// <pkg>/lib/tsserver.js -> <pkg>
		roots = append(roots, PeekRoot{Path: filepath.Dir(filepath.Dir(tsserver)), Label: "$TYPESCRIPT"})
	}
	if dir := npmGlobalRoot(); dir != "" {
		roots = append(roots, PeekRoot{Path: dir, Label: "$NPM_GLOBAL"})
	}
	return roots
}

func pathExt(path string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
}

// LanguageForPath returns nil when no registered language covers path.
func LanguageForPath(path string) *Language {
	ext := pathExt(path)
	if ext == "" {
		return nil
	}
	for _, l := range languages {
		if _, ok := l.IDByExt[ext]; ok {
			return l
		}
	}
	return nil
}

// LanguageID returns the LSP languageId to send with didOpen for path.
func (l *Language) LanguageID(path string) string {
	return l.IDByExt[pathExt(path)]
}

// availableTTL bounds how long a PATH lookup is reused. /api/config asks
// about every language on each request, and on Windows each exec.LookPath
// walks PATH × PATHEXT (~13ms on a CI runner, against 0.05ms on Linux). A
// server installed mid-session still shows up within this window.
const availableTTL = 30 * time.Second

// availableCache maps PATH + "\x00" + binary to an availableEntry; keying on
// PATH keeps a changed PATH from being answered from a stale lookup.
var availableCache sync.Map

type availableEntry struct {
	ok bool
	at time.Time
}

// Available reports whether the language's server binary is on PATH and,
// for a language with Requires, that what it needs beyond the binary is
// installed too.
func (l *Language) Available() bool {
	return l.availableAt(time.Now())
}

func (l *Language) availableAt(now time.Time) bool {
	key := os.Getenv("PATH") + "\x00" + l.Command[0]
	if v, ok := availableCache.Load(key); ok {
		if e := v.(availableEntry); now.Sub(e.at) < availableTTL {
			return e.ok
		}
	}
	_, err := exec.LookPath(l.Command[0])
	ok := err == nil
	if ok && l.Requires != nil {
		ok = l.Requires("") == nil
	}
	availableCache.Store(key, availableEntry{ok: ok, at: now})
	return ok
}

// Any reports whether include matches at least one registered language.
func Any(include func(*Language) bool) bool {
	return slices.ContainsFunc(languages, include)
}

// Extensions returns the sorted extensions (no dots) of every language
// matched by include.
func Extensions(include func(*Language) bool) []string {
	var exts []string
	for _, l := range languages {
		if !include(l) {
			continue
		}
		for ext := range l.IDByExt {
			exts = append(exts, ext)
		}
	}
	sort.Strings(exts)
	return exts
}

// SparsePatternsForFiles unions the patterns of the languages that cover at
// least one of paths and match include. Content-based on purpose: a language
// installed on the machine but absent from the review must not inflate the
// checkout (or its size estimate against lsp_worktree_max_mb).
func SparsePatternsForFiles(paths []string, include func(*Language) bool) []string {
	need := make(map[string]bool)
	for _, p := range paths {
		if l := LanguageForPath(p); l != nil {
			need[l.Name] = true
		}
	}
	var patterns []string
	for _, l := range languages {
		if need[l.Name] && include(l) {
			patterns = append(patterns, l.SparsePatterns...)
		}
	}
	return patterns
}
