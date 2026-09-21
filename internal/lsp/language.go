package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// PeekRoot is one directory tree outside the workspace root that definition
// and reference peeks are allowed to read, with the display prefix the UI
// shows for paths under it.
type PeekRoot struct {
	Path  string // absolute directory
	Label string // display prefix, e.g. "$GOROOT"
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
	// IDByExt maps a file extension (lower-case, no dot) to the LSP
	// languageId sent with didOpen.
	IDByExt map[string]string
	// SparsePatterns is the sparse-checkout pattern set for the range-focus
	// LSP worktree: source and project files, enough for the server without
	// the rest of the tree.
	SparsePatterns []string
	// InitOptions returns the initializationOptions to send with initialize
	// for a server rooted at root whose first request is about absPath, or
	// nil to send none. Runs on the request path (once per server spawn) —
	// keep it filesystem-cheap.
	InitOptions func(root, absPath string) map[string]any
	// ConfigSettings returns the settings the server pulls with
	// workspace/configuration, keyed by section name ("python"), for a server
	// rooted at root whose first request is about absPath, or nil when the
	// language needs none. Some servers take settings only this way — pyright
	// ignores initializationOptions and reads python.* from the pull — so this
	// is separate from InitOptions. A nil result also leaves the capability
	// undeclared, so a server that never needed it is never asked. Same cost
	// rule as InitOptions: it runs once per server spawn, keep it
	// filesystem-cheap, and never execute anything the repo supplies.
	ConfigSettings func(root, absPath string) map[string]any
	// SkipReadyWait skips the wait for startup progress after a document is
	// opened (see Client.WaitReady). Right for a server that queues requests
	// behind its own analysis and so never answers from a half-built project
	// — pyright — and reports no progress for that analysis: the wait would
	// only sit out its whole grace period. Wrong for one that answers early
	// and wrong, like typescript-language-server.
	SkipReadyWait bool
	// ExtraRoots resolves the language's out-of-workspace source roots where
	// definitions can land (e.g. GOROOT for Go, the global node_modules for
	// TypeScript). root is the tree the language's dependencies live in — the
	// workspace root, or the working tree backing it under range/PR focus —
	// for languages whose roots depend on the project rather than the
	// machine. May run external commands; the Manager caches successful
	// results per root. A nil result means the lookup failed and may be
	// retried.
	ExtraRoots func(root string) []PeekRoot
}

// languages is the registry of supported language servers.
//
// TypeScript note: node_modules is not tracked by git, so a range-focus
// sparse worktree has no dependencies — cross-package hover/definition
// degrades there, while intra-repo symbols keep working. The normal
// working-tree focus resolves node_modules as usual.
//
// Python note: the same holds for a virtualenv (.venv is untracked), and
// Manager.depRoot covers it the same way. pyright does not discover a .venv
// on its own, so pyConfigSettings tells it where the packages are.
var languages = []*Language{
	{
		Name:    "go",
		Command: []string{"gopls"},
		IDByExt: map[string]string{"go": "go"},
		SparsePatterns: []string{
			"*.go", "go.mod", "go.sum", "go.work", "go.work.sum",
		},
		ExtraRoots: goExtraRoots,
	},
	{
		Name:    "typescript",
		Command: []string{"typescript-language-server", "--stdio"},
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
		ExtraRoots:  npmGlobalRoots,
	},
	{
		Name:    "python",
		Command: []string{"pyright-langserver", "--stdio"},
		IDByExt: map[string]string{"py": "python", "pyi": "python"},
		SparsePatterns: []string{
			"*.py", "*.pyi", "pyproject.toml", "pyrightconfig.json",
		},
		ConfigSettings: pyConfigSettings,
		SkipReadyWait:  true,
		ExtraRoots:     pyExtraRoots,
	},
}

// tsInitOptions pins the TypeScript installation typescript-language-server
// should run. The server resolves the "typescript" package from its workspace
// root and exits during initialize when it finds none — which is the ordinary
// layout in a monorepo, where the dependency belongs to the package that owns
// the file (e.g. frontend/node_modules/typescript) and not to the repo root
// crit anchors the workspace to. So walk up from the file and pin the nearest
// install. Returning nil leaves the server's own resolution in charge, which
// is right for a single-package repo or a global typescript.
func tsInitOptions(root, absPath string) map[string]any {
	tsserver := findTSServer(root, absPath)
	if tsserver == "" {
		return nil
	}
	return map[string]any{"tsserver": map[string]any{"path": tsserver}}
}

// findTSServer returns the tsserver.js of the node_modules/typescript nearest
// to absPath, searching its directory upwards through root (inclusive), or ""
// when there is none. The search never leaves root: a file outside it is not
// ours to resolve dependencies for.
func findTSServer(root, absPath string) string {
	root = filepath.Clean(root)
	dir := filepath.Dir(absPath)
	if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	for {
		candidate := filepath.Join(dir, "node_modules", "typescript", "lib", "tsserver.js")
		// Stat, not Lstat: pnpm and Yarn link the package into the consuming
		// package's node_modules, and the link target is the real install.
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
		if dir == root {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// goExtraRoots resolves GOROOT and GOMODCACHE, where Go definitions outside
// the repo land (stdlib, module cache). Both are machine-wide, so the
// workspace root is not consulted.
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

// npmGlobalRoots resolves the global node_modules directory, where the
// typescript lib.*.d.ts files land when typescript-language-server and
// typescript are installed globally (the README's install command) and the
// repo has no local typescript dependency. The global root is machine-wide,
// so the workspace root is not consulted.
func npmGlobalRoots(_ string) []PeekRoot {
	out, err := exec.Command("npm", "root", "-g").Output()
	if err != nil {
		return nil
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return nil
	}
	return []PeekRoot{{Path: dir, Label: "$NPM_GLOBAL"}}
}

// pathExt returns the lower-case extension of path without the dot.
func pathExt(path string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
}

// LanguageForPath returns the registered language covering path's extension,
// or nil when no language server handles it.
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

// Available reports whether the language's server binary is on PATH.
func (l *Language) Available() bool {
	_, err := exec.LookPath(l.Command[0])
	return err == nil
}

// Any reports whether include matches at least one registered language.
// Callers pass their availability predicate — the real PATH lookup, or a
// test stub.
func Any(include func(*Language) bool) bool {
	for _, l := range languages {
		if include(l) {
			return true
		}
	}
	return false
}

// Extensions returns the sorted extensions (no dots) of every language
// matched by include. The frontend uses this to decide which files get
// hover/definition affordances.
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

// SparsePatternsForFiles returns the union of sparse-checkout patterns for
// the languages that cover at least one of paths AND are matched by include,
// for the range-focus LSP worktree. Content-based on purpose: a language
// that is installed on the machine but absent from the review must not
// inflate the checkout (or its size estimate against lsp_worktree_max_mb)
// with files its server will never be asked about.
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
