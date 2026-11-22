package lsp

import (
	"os"
	"path/filepath"
	"runtime"
)

// dartArgs moves the analysis server's cache from ~/.dartServer, which the
// sandbox cannot write, into the scratch, where it survives restarts.
func dartArgs(ws Workspace) []string {
	return []string{"--cache=" + filepath.Join(ws.Scratch, "cache")}
}

func dartExtraRoots(root string, _ Run) []PeekRoot {
	var roots []PeekRoot
	if sdk := dartSDKRoot(root); sdk != "" {
		roots = append(roots, PeekRoot{Path: filepath.Join(sdk, "lib"), Label: "$DART_SDK"})
	}
	if cache := pubCacheDir(); cache != "" {
		roots = append(roots, PeekRoot{Path: cache, Label: "$PUB_CACHE", LocalEnv: true})
	}
	if len(roots) == 0 {
		return nil
	}
	return roots
}

func dartSDKRoot(tree string) string {
	bin := serverBinary("dart", tree)
	if bin == "" {
		return ""
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return ""
	}
	return dartSDKRootFor(real)
}

// dartSDKRootFor knows two layouts around a dart binary: <sdk>/bin/dart for a
// plain SDK, and Flutter's <flutter>/bin/dart, a wrapper script that runs
// <flutter>/bin/cache/dart-sdk/bin/dart.
func dartSDKRootFor(bin string) string {
	dir := filepath.Dir(bin)
	for _, sdk := range []string{filepath.Dir(dir), filepath.Join(dir, "cache", "dart-sdk")} {
		if st, err := os.Stat(filepath.Join(sdk, "lib", "core", "core.dart")); err == nil && !st.IsDir() {
			return sdk
		}
	}
	return ""
}

func pubCacheDir() string {
	dir := os.Getenv("PUB_CACHE")
	if dir == "" {
		if runtime.GOOS == "windows" {
			if local := os.Getenv("LOCALAPPDATA"); local != "" {
				dir = filepath.Join(local, "Pub", "Cache")
			}
		} else if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".pub-cache")
		}
	}
	if dir == "" || !isDir(dir) {
		return ""
	}
	return dir
}
