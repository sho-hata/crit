package lsp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sho-hata/crit/internal/sandbox"
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

const dartPkgMainSource = `import 'package:contract/greeter.dart';

void main() {
  print(greet('world'));
}
`

const dartPackageConfig = `{
  "configVersion": 2,
  "packages": [
    {"name": "contract", "rootUri": "../", "packageUri": "lib/", "languageVersion": "3.0"}
  ],
  "generator": "pub"
}
`

const (
	dartGreetUseLine, dartGreetUseChar = 3, 8
	dartPrintLine, dartPrintChar       = 3, 2
)

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

func writeDartProject(t *testing.T, dir string) string {
	t.Helper()
	writeTree(t, dir, map[string]string{
		"pubspec.yaml":     dartPubspecSource,
		"lib/greeter.dart": dartGreeterSource,
		"lib/main.dart":    dartMainSource,
	})
	return filepath.Join(dir, "lib", "main.dart")
}

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

func TestRealDartNeverRunsAnalyzerPlugins(t *testing.T) {
	t.Parallel()
	requireRealDart(t)

	dir := t.TempDir()
	main := writeDartProject(t, dir)
	marker := writeDartPluginCanary(t, dir)
	before := listTree(t, dir)

	m := NewManager(dir, "", t.TempDir(), context.Background())
	t.Cleanup(m.Shutdown)

	got, err := m.Hover(main, dartGreetUseLine, dartGreetUseChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if !strings.Contains(got, "friendly greeting") {
		t.Errorf("hover = %q, want the doc comment", got)
	}
	if ran, ok := waitForMarker(marker, 5*time.Second); ok {
		t.Errorf("the analyzer plugin was run: %q", ran)
	}
	m.Shutdown()

	if after := listTree(t, dir); !slices.Equal(after, before) {
		t.Errorf("repo files changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestRealDartRunsAnalyzerPluginsByDefault(t *testing.T) {
	requireRealDart(t)
	if sb := sandbox.Detect(); sb.Name != "" {
		t.Skipf("%s denies the plugin's pub fetch and its marker write; the control needs an unconfined server", sb.Name)
	}
	t.Setenv("PUB_CACHE", t.TempDir())

	dir := t.TempDir()
	main := writeDartProject(t, dir)
	marker := writeDartPluginCanary(t, dir)

	lang := *LanguageForPath(main)
	lang.Command = slices.DeleteFunc(slices.Clone(lang.Command), func(a string) bool { return a == "--no-plugins" })

	m := NewManager(dir, "", t.TempDir(), context.Background())
	c, err := m.startServer(context.Background(), &lang, nil, nil)
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

func TestRealDartSDKDefinitionIsPeekable(t *testing.T) {
	t.Parallel()
	requireRealDart(t)

	dir := t.TempDir()
	main := writeDartProject(t, dir)

	m := NewManager(dir, "", t.TempDir(), context.Background())
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

			m := NewManager(dir, "", t.TempDir(), context.Background())
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

func TestRealDartKeepsItsCacheInTheScratch(t *testing.T) {
	t.Parallel()
	requireRealDart(t)

	dir := t.TempDir()
	main := writeDartProject(t, dir)
	m := NewManager(dir, "", t.TempDir(), context.Background())
	t.Cleanup(m.Shutdown)

	if _, err := m.Hover(main, dartGreetUseLine, dartGreetUseChar); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	cache := filepath.Join(testSandbox(t, m).Workspace("dart").Scratch, "cache")
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) == 0 {
		t.Fatalf("nothing cached under %s: %v", cache, err)
	}
}
