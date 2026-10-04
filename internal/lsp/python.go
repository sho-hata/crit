package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// venvDirs is in preference order.
var venvDirs = []string{".venv", "venv"}

// pyConfigSettings tells pyright where a project's third-party packages live.
//
// pyright does not find a virtualenv on its own: without this, imports that
// resolve into a .venv come back Unknown (no type on hover, definitions land
// on the import line) and nothing reports an error. It reads the answer from a
// workspace/configuration request, not initializationOptions.
//
// It sends python.analysis.extraPaths, not python.pythonPath: pythonPath makes
// pyright execute that interpreter, and here it would come from the tree under
// review, so a branch committing .venv/bin/python would run code just by being
// opened in a diff. A directory is safe. extraPaths cannot carry what only the
// interpreter knows (.pth files, the Python version); an activated venv on
// PATH and a pyrightconfig.json venv are handled by pyright itself.
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

// findSitePackages walks up from absPath's directory through root
// (inclusive), like findTSServer. The nearest venv wins because a monorepo
// keeps one per service.
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

func venvSitePackages(dir string) string {
	for _, name := range venvDirs {
		if sp := sitePackagesOf(filepath.Join(dir, name)); sp != "" {
			return sp
		}
	}
	return ""
}

// sitePackagesOf knows two layouts: Lib\site-packages on Windows and
// lib/python3.X/site-packages elsewhere — a venv holds exactly one Python
// version, so the first match is the only one.
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

const pythonEnvScript = `import sysconfig; p = sysconfig.get_paths(); print(p["stdlib"]); print(p["purelib"])`

// pyExtraRoots:
//   - the virtualenv in root (under range/PR focus the working tree, outside
//     the sparse worktree the server runs in)
//   - the PATH interpreter's stdlib and site-packages
//   - pyright's bundled typeshed: a definition into the stdlib answers with
//     both the .pyi there and the real .py
//
// Each source is optional; only a lookup that finds nothing at all is nil.
func pyExtraRoots(root string) []PeekRoot {
	var roots []PeekRoot
	if sp := venvSitePackages(root); sp != "" {
		roots = append(roots, PeekRoot{Path: sp, Label: "$SITE_PACKAGES", LocalEnv: true})
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

// pythonEnvRoots asks the PATH interpreter — the one pyright itself falls
// back to, never a repo-local one.
func pythonEnvRoots() []PeekRoot {
	for _, bin := range []string{"python3", "python"} {
		// -I (isolated mode) is required: without it the interpreter puts the
		// working directory first on sys.path, and the daemon runs inside the
		// repo under review, so a committed sysconfig.py would run.
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

func parsePythonEnv(out string) []PeekRoot {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// Only site-packages is the local environment: the stdlib is the same
	// for every checkout of the code.
	labels := []PeekRoot{{Label: "$PYTHON_STDLIB"}, {Label: "$SITE_PACKAGES", LocalEnv: true}}
	var roots []PeekRoot
	for i, root := range labels {
		if i >= len(lines) {
			break
		}
		if dir := strings.TrimSpace(lines[i]); dir != "" {
			root.Path = dir
			roots = append(roots, root)
		}
	}
	return roots
}

// pyrightTypeshed returns "" when the binary is not laid out like the npm
// package; stdlib stub peeks are then unreadable, the real stdlib still opens.
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
