package lsp

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

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
}

// languages is the registry of supported language servers.
//
// TypeScript note: node_modules is not tracked by git, so a range-focus
// sparse worktree has no dependencies — cross-package hover/definition
// degrades there, while intra-repo symbols keep working. The normal
// working-tree focus resolves node_modules as usual.
var languages = []*Language{
	{
		Name:    "go",
		Command: []string{"gopls"},
		IDByExt: map[string]string{"go": "go"},
		SparsePatterns: []string{
			"*.go", "go.mod", "go.sum", "go.work", "go.work.sum",
		},
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
	},
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

// AnyAvailable reports whether at least one registered language server is on
// PATH.
func AnyAvailable() bool {
	for _, l := range languages {
		if l.Available() {
			return true
		}
	}
	return false
}

// AvailableExtensions returns the sorted extensions (no dots) of every
// language whose server binary is on PATH. The frontend uses this to decide
// which files get hover/definition affordances.
func AvailableExtensions() []string {
	return extensions(func(l *Language) bool { return l.Available() })
}

// AllExtensions returns the sorted extensions of every registered language,
// regardless of binary availability. Used by tests that stub the PATH lookup.
func AllExtensions() []string {
	return extensions(func(*Language) bool { return true })
}

func extensions(include func(*Language) bool) []string {
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

// SparsePatterns returns the union of sparse-checkout patterns for every
// language whose server binary is on PATH, for the range-focus LSP worktree.
func SparsePatterns() []string {
	return sparsePatterns(func(l *Language) bool { return l.Available() })
}

// AllSparsePatterns returns the union of sparse-checkout patterns for every
// registered language, regardless of binary availability. Used by tests that
// stub the PATH lookup.
func AllSparsePatterns() []string {
	return sparsePatterns(func(*Language) bool { return true })
}

func sparsePatterns(include func(*Language) bool) []string {
	var patterns []string
	for _, l := range languages {
		if include(l) {
			patterns = append(patterns, l.SparsePatterns...)
		}
	}
	return patterns
}
