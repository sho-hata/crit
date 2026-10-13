package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// dartExtraRoots:
//   - the SDK's lib/, where definitions of dart: library members (print,
//     String) land
//   - the pub cache, where hosted packages live; the repo's
//     .dart_tool/package_config.json points package: imports there
//
// Both are machine-wide, so root is ignored. The cache holds whatever the
// reviewer has fetched, not the reviewed SHA's versions, hence LocalEnv.
// Each source is optional; only a lookup that finds nothing at all is nil.
func dartExtraRoots(_ string) []PeekRoot {
	var roots []PeekRoot
	if sdk := dartSDKRoot(); sdk != "" {
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

// dartSDKRoot is the SDK behind the dart on PATH, "" when its layout is not
// recognized (built-in definitions then still answer, without a peek).
func dartSDKRoot() string {
	bin, err := exec.LookPath("dart")
	if err != nil {
		return ""
	}
	// Package managers link the binary; the SDK sits around the target.
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return ""
	}
	return dartSDKRootFor(real)
}

// dartSDKRootFor knows two layouts around a dart binary: <sdk>/bin/dart for a
// plain SDK, and Flutter's <flutter>/bin/dart, a wrapper script that runs
// <flutter>/bin/cache/dart-sdk/bin/dart. lib/core/core.dart is what makes a
// candidate an SDK, so an unknown layout yields "" rather than a wrong root.
func dartSDKRootFor(bin string) string {
	dir := filepath.Dir(bin)
	for _, sdk := range []string{filepath.Dir(dir), filepath.Join(dir, "cache", "dart-sdk")} {
		if st, err := os.Stat(filepath.Join(sdk, "lib", "core", "core.dart")); err == nil && !st.IsDir() {
			return sdk
		}
	}
	return ""
}

// pubCacheDir is pub's system cache: $PUB_CACHE, else ~/.pub-cache
// (%LOCALAPPDATA%\Pub\Cache on Windows). "" until a package has been fetched
// into it: a peek root should exist.
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
