package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// venvDirs are the conventional in-tree virtualenv directory names, in
// preference order.
var venvDirs = []string{".venv", "venv"}

// pyConfigSettings tells pyright where a project's third-party packages live.
//
// pyright does not discover a virtualenv on its own: without this, every
// import that resolves into a .venv comes back Unknown, hovers carry no type
// and definitions land on the import statement. Nothing errors, so nothing
// retries. It reads the answer from a workspace/configuration pull — it
// ignores initializationOptions — as python.analysis.extraPaths.
//
// extraPaths, not python.pythonPath, on purpose. pythonPath makes pyright
// execute that interpreter to learn its search paths, and here the interpreter
// would come from the tree under review: a branch that commits
// .venv/bin/python would run code just by being opened in a diff. extraPaths
// is a directory and nothing else. What it does not carry is what only the
// interpreter knows (.pth files, the exact Python version); an activated venv
// is already first on PATH, which pyright follows by itself, and a project's
// own pyrightconfig.json venv is read by pyright directly.
//
// Returns nil when there is no in-tree virtualenv, which leaves everything to
// pyright's own resolution.
func pyConfigSettings(root, absPath string) map[string]any {
	sp := findSitePackages(root, absPath)
	if sp == "" {
		return nil
	}
	return map[string]any{
		"python": map[string]any{
			"analysis": map[string]any{"extraPaths": []string{sp}},
		},
	}
}

// findSitePackages returns the site-packages of the virtualenv nearest to
// absPath, searching its directory upwards through root (inclusive), or ""
// when there is none. The nearest wins because a monorepo keeps one venv per
// service. The search never leaves root: a file outside it is not ours to
// resolve dependencies for.
func findSitePackages(root, absPath string) string {
	root = filepath.Clean(root)
	dir := filepath.Dir(absPath)
	if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	for {
		if sp := venvSitePackages(dir); sp != "" {
			return sp
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

// venvSitePackages returns the site-packages of the conventional virtualenv
// sitting directly in dir, or "".
func venvSitePackages(dir string) string {
	for _, name := range venvDirs {
		if sp := sitePackagesOf(filepath.Join(dir, name)); sp != "" {
			return sp
		}
	}
	return ""
}

// sitePackagesOf returns venv's site-packages directory, or "" when venv is
// not a virtualenv. Two layouts: Lib\site-packages on Windows and
// lib/python3.X/site-packages everywhere else — a venv holds exactly one
// Python version, so the first match is the only one.
func sitePackagesOf(venv string) string {
	if sp := filepath.Join(venv, "Lib", "site-packages"); isDir(sp) {
		return sp
	}
	entries, err := os.ReadDir(filepath.Join(venv, "lib"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "python3") {
			continue
		}
		if sp := filepath.Join(venv, "lib", e.Name(), "site-packages"); isDir(sp) {
			return sp
		}
	}
	return ""
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// pythonEnvScript prints the interpreter's stdlib and purelib directories.
const pythonEnvScript = `import sysconfig; p = sysconfig.get_paths(); print(p["stdlib"]); print(p["purelib"])`

// pyExtraRoots resolves where Python definitions outside the repo land:
//   - the project's virtualenv, when it is not already under the LSP root
//     (range/PR focus roots the server at a sparse worktree, and the venv
//     stays in the working tree that root is given as)
//   - the PATH interpreter's stdlib and site-packages
//   - the typeshed bundled with pyright, where stdlib stubs live: a
//     definition into the stdlib answers with both the .pyi there and the
//     real .py
//
// Each source is optional; only a lookup that finds nothing at all is nil.
func pyExtraRoots(root string) []PeekRoot {
	var roots []PeekRoot
	if sp := venvSitePackages(root); sp != "" {
		roots = append(roots, PeekRoot{Path: sp, Label: "$SITE_PACKAGES"})
	}
	roots = append(roots, pythonEnvRoots()...)
	if dir := pyrightTypeshed(); dir != "" {
		roots = append(roots, PeekRoot{Path: dir, Label: "$TYPESHED"})
	}
	if len(roots) == 0 {
		return nil
	}
	return roots
}

// pythonEnvRoots asks the interpreter on PATH for its stdlib and site-packages.
// That is the interpreter pyright itself falls back to, so the paths it
// reports are the ones its definitions land in. Never a repo-local
// interpreter: only PATH is consulted.
func pythonEnvRoots() []PeekRoot {
	for _, bin := range []string{"python3", "python"} {
		// -I (isolated mode) is not decoration. Without it the interpreter puts
		// the working directory first on sys.path, and the daemon runs inside
		// the repo under review — a sysconfig.py committed there would run.
		out, err := exec.Command(bin, "-I", "-c", pythonEnvScript).Output()
		if err != nil {
			continue
		}
		if roots := parsePythonEnv(string(out)); roots != nil {
			return roots
		}
	}
	return nil
}

// parsePythonEnv turns pythonEnvScript's output — stdlib, then purelib, one
// per line — into peek roots.
func parsePythonEnv(out string) []PeekRoot {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	labels := []string{"$PYTHON_STDLIB", "$SITE_PACKAGES"}
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

// pyrightTypeshed locates the typeshed stubs bundled with the installed
// pyright, or "" when they cannot be found (a pip-installed wrapper keeps them
// in its own cache; peeks into stdlib stubs then fall outside the readable
// roots, while the real stdlib source still opens).
func pyrightTypeshed() string {
	bin, err := exec.LookPath("pyright-langserver")
	if err != nil {
		return ""
	}
	// npm links the bin into <prefix>/bin; the package, and the typeshed in
	// its dist/, sits next to the real file the link resolves to.
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return ""
	}
	dir := filepath.Join(filepath.Dir(real), "dist", "typeshed-fallback")
	if isDir(dir) {
		return dir
	}
	return ""
}
