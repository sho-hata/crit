package lsp

// Opt-in integration tests against a real Dart analysis server (same
// CRIT_LSP_REAL gate as the other real-server tests). Skipped unless enabled:
//
//	CRIT_LSP_REAL=1 go test ./internal/lsp -run TestRealDart -v
//
// Requires dart on PATH (the analysis server ships inside the SDK). The
// plugin control fetches analysis_server_plugin from pub.dev, so it needs
// network — as does scripts/install-lsp-servers.sh.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const dartPubspecSource = "name: contract\nenvironment:\n  sdk: ^3.0.0\n"

const dartGreeterSource = `/// Returns a friendly greeting for name.
String greet(String name) => 'Hello, $name';
`

const dartMainSource = `import 'greeter.dart';

void main() {
  print(greet('world'));
}
`

// dartPkgMainSource imports the own package by package: URI, the usual Dart
// style, which resolves only through .dart_tool/package_config.json.
const dartPkgMainSource = `import 'package:contract/greeter.dart';

void main() {
  print(greet('world'));
}
`

// dartPackageConfig is what `dart pub get` writes for a package without
// dependencies, trimmed to what the analyzer reads.
const dartPackageConfig = `{
  "configVersion": 2,
  "packages": [
    {"name": "contract", "rootUri": "../", "packageUri": "lib/", "languageVersion": "3.0"}
  ],
  "generator": "pub"
}
`

// Positions (0-based line, UTF-16 character); the same in dartMainSource and
// dartPkgMainSource.
const (
	dartGreetUseLine, dartGreetUseChar = 3, 8 // greet
	dartPrintLine, dartPrintChar       = 3, 2 // print
)

// dartPluginRunWindow is how long to wait for the server to fetch and start
// the canary plugin. The control sees it in about 20s: the plugin's
// dependencies come from pub.dev.
const dartPluginRunWindow = 90 * time.Second

func requireRealDart(t *testing.T) {
	t.Helper()
	if os.Getenv("CRIT_LSP_REAL") == "" {
		t.Skip("set CRIT_LSP_REAL=1 to run against a real Dart analysis server")
	}
	if !LanguageForPath("x.dart").Available() {
		t.Fatal("dart not on PATH")
	}
}

// writeDartProject lays out the contract package under dir and returns
// lib/main.dart's path.
func writeDartProject(t *testing.T, dir string) string {
	t.Helper()
	writeTree(t, dir, map[string]string{
		"pubspec.yaml":     dartPubspecSource,
		"lib/greeter.dart": dartGreeterSource,
		"lib/main.dart":    dartMainSource,
	})
	return filepath.Join(dir, "lib", "main.dart")
}

// writeDartPluginCanary adds an analyzer plugin to the project in dir, the
// way a branch would commit one: analysis_options.yaml names it, and its
// package lives in the tree. Loading the plugin writes marker; the path is
// baked in because the plugin runs as an isolate of the server. Returns the
// marker path.
func writeDartPluginCanary(t *testing.T, dir string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "plugin-ran")
	writeTree(t, dir, map[string]string{
		"analysis_options.yaml": "plugins:\n  canary:\n    path: tool/canary\n",
		"tool/canary/pubspec.yaml": "name: canary\nenvironment:\n  sdk: ^3.0.0\n" +
			"dependencies:\n  analysis_server_plugin: any\n",
		"tool/canary/lib/main.dart": "import 'dart:io';\n" +
			"import 'package:analysis_server_plugin/plugin.dart';\n\n" +
			"final Plugin plugin = _mark();\n\n" +
			"Plugin _mark() {\n" +
			"  File(r'" + marker + "').writeAsStringSync('ran');\n" +
			"  throw StateError('canary');\n}\n",
	})
	return marker
}

// A repo's analysis_options.yaml can name analyzer plugins, which the server
// fetches with `dart pub` and runs in-process. crit must keep it from doing
// so, and the hover must still answer.
func TestRealDartNeverRunsAnalyzerPlugins(t *testing.T) {
	t.Parallel()
	requireRealDart(t)

	dir := t.TempDir()
	main := writeDartProject(t, dir)
	marker := writeDartPluginCanary(t, dir)
	before := listTree(t, dir)

	m := NewManager(dir, "", context.Background())
	t.Cleanup(m.Shutdown) // a failed Hover must not leak the server

	got, err := m.Hover(main, dartGreetUseLine, dartGreetUseChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if !strings.Contains(got, "friendly greeting") {
		t.Errorf("hover = %q, want the doc comment", got)
	}
	// With plugins off the server never schedules the fetch, so a short
	// window is enough: the control below sees the plugin manager at work
	// (its pub fetch, then the marker) within seconds of the didOpen.
	if ran, ok := waitForMarker(marker, 5*time.Second); ok {
		t.Errorf("the analyzer plugin was run: %q", ran)
	}
	// Stop it before listing: a still-running server could write afterwards.
	m.Shutdown()

	if after := listTree(t, dir); !slices.Equal(after, before) {
		t.Errorf("repo files changed:\nbefore %v\nafter  %v", before, after)
	}
}

// Control: the same project with the server's own defaults does fetch and run
// the plugin, so the test above is not passing vacuously. Not parallel: it
// points PUB_CACHE, which the server inherits, at a temp dir so the fetch
// leaves nothing behind.
func TestRealDartRunsAnalyzerPluginsByDefault(t *testing.T) {
	requireRealDart(t)
	t.Setenv("PUB_CACHE", t.TempDir())

	dir := t.TempDir()
	main := writeDartProject(t, dir)
	marker := writeDartPluginCanary(t, dir)

	lang := *LanguageForPath(main)
	lang.Command = slices.DeleteFunc(slices.Clone(lang.Command), func(a string) bool { return a == "--no-plugins" })

	c, err := startServer(context.Background(), dir, &lang, nil, nil)
	if err != nil {
		t.Fatalf("startServer: %v", err)
	}
	defer c.Close()
	if err := c.DidOpen(main, "dart", dartMainSource, 1); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	start := time.Now()
	if _, ok := waitForMarker(marker, dartPluginRunWindow); !ok {
		t.Fatal("the analyzer plugin was never run; the fixture no longer triggers it (or pub.dev is unreachable)")
	}
	t.Logf("the plugin ran %s after the didOpen", time.Since(start).Round(time.Millisecond))
}

// Definitions of dart: library members land in the SDK.
func TestRealDartSDKDefinitionIsPeekable(t *testing.T) {
	t.Parallel()
	requireRealDart(t)

	dir := t.TempDir()
	main := writeDartProject(t, dir)

	m := NewManager(dir, "", context.Background())
	defer m.Shutdown()

	locs, err := m.Definition(main, dartPrintLine, dartPrintChar)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) == 0 {
		t.Fatal("no definition for print")
	}
	roots := m.PeekRoots()
	for _, l := range locs {
		if !underPeekRoot(l.Path, roots) {
			t.Errorf("definition %s is not under any peek root %+v", l.Path, roots)
		}
	}
}

// package: imports resolve through .dart_tool/package_config.json and not
// otherwise — which is what a range-focus worktree, where that untracked
// file is absent, gets.
func TestRealDartPackageImportNeedsPackageConfig(t *testing.T) {
	t.Parallel()
	requireRealDart(t)

	cases := []struct {
		name       string
		withConfig bool
	}{
		{"with package_config", true},
		{"without package_config", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			files := map[string]string{
				"pubspec.yaml":     dartPubspecSource,
				"lib/greeter.dart": dartGreeterSource,
				"bin/main.dart":    dartPkgMainSource,
			}
			if tc.withConfig {
				files[".dart_tool/package_config.json"] = dartPackageConfig
			}
			writeTree(t, dir, files)
			main := filepath.Join(dir, "bin", "main.dart")

			m := NewManager(dir, "", context.Background())
			defer m.Shutdown()

			got, err := m.Hover(main, dartGreetUseLine, dartGreetUseChar)
			if err != nil {
				t.Fatalf("Hover: %v", err)
			}
			locs, err := m.Definition(main, dartGreetUseLine, dartGreetUseChar)
			if err != nil {
				t.Fatalf("Definition: %v", err)
			}
			if resolved := strings.Contains(got, "friendly greeting"); resolved != tc.withConfig {
				t.Errorf("hover = %q, want resolved=%v", got, tc.withConfig)
			}
			if gotDef := len(locs) > 0; gotDef != tc.withConfig {
				t.Errorf("definition = %+v, want one=%v", locs, tc.withConfig)
			}
		})
	}
}
