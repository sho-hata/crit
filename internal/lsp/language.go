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

// node_modules and .venv are untracked, so a range-focus sparse worktree has
// neither. Manager.depRoot borrows the working tree's for the handshake (the
// tsserver to pin, the site-packages for pyright); TypeScript cross-package
// hover/definition still degrades there, intra-repo symbols keep working.
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
		LocalEnv:       true,
		SkipReadyWait:  true,
		ExtraRoots:     pyExtraRoots,
	},
}

// tsInitOptions pins the nearest TypeScript install. typescript-language-server
// resolves "typescript" from its workspace root and exits during initialize
// when it finds none — the ordinary monorepo layout, where the dependency
// belongs to the owning package (frontend/node_modules/typescript), not the
// repo root crit anchors the workspace to. nil leaves the server's own
// resolution in charge (single-package repo, global typescript).
func tsInitOptions(root, absPath string) map[string]any {
	tsserver := findTSServer(root, absPath)
	if tsserver == "" {
		return nil
	}
	return map[string]any{"tsserver": map[string]any{"path": tsserver}}
}

// findTSServer walks up from absPath's directory through root (inclusive).
// It never leaves root: a file outside it is not ours to resolve
// dependencies for.
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

// npmGlobalRoots is where lib.*.d.ts definitions land when typescript is
// installed globally (the README's install command) and the repo has no
// local typescript. Machine-wide, so root is ignored.
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

// Available reports whether the language's server binary is on PATH.
func (l *Language) Available() bool {
	_, err := exec.LookPath(l.Command[0])
	return err == nil
}

// Any reports whether include matches at least one registered language.
func Any(include func(*Language) bool) bool {
	for _, l := range languages {
		if include(l) {
			return true
		}
	}
	return false
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
