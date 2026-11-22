package lsp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

const javaGreeterSource = `package com.example;

/** A friendly greeter. */
public class Greeter {
    /** Returns a friendly greeting for name. */
    public static String greet(String name) {
        return "Hello, " + name;
    }
}
`

const javaAppSource = `package com.example;

public class App {
    public static void main(String[] args) {
        System.out.println(Greeter.greet("world"));
    }
}
`

var (
	javaGreetUseLine, javaGreetUseChar = sourcePos(javaAppSource, "greet(")
	javaGreetDefLine, _                = sourcePos(javaGreeterSource, "public static String greet")
	javaPrintlnLine, javaPrintlnChar   = sourcePos(javaAppSource, "println")
)

func sourcePos(src, needle string) (line, char int) {
	i := strings.Index(src, needle)
	if i < 0 {
		panic("fixture has no " + needle)
	}
	before := src[:i]
	lineStart := strings.LastIndex(before, "\n") + 1
	return strings.Count(before, "\n"), len(utf16.Encode([]rune(before[lineStart:])))
}

func requireRealJdtls(t *testing.T) {
	t.Helper()
	if os.Getenv("CRIT_LSP_REAL") == "" {
		t.Skip("set CRIT_LSP_REAL=1 to run against a real jdt.ls")
	}
	if !LanguageForPath("x.java").Available() {
		t.Fatal("jdtls not on PATH")
	}
}

func writeJavaProject(t *testing.T, dir string) (app, marker string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "gradle-ran")
	writeTree(t, dir, map[string]string{
		"src/main/java/com/example/Greeter.java": javaGreeterSource,
		"src/main/java/com/example/App.java":     javaAppSource,
		"settings.gradle": "new File('" + filepath.ToSlash(marker) + "').text = 'ran'\n" +
			"rootProject.name = 'canary'\n",
		"build.gradle": "plugins { id 'java' }\n",
	})
	return filepath.Join(dir, "src", "main", "java", "com", "example", "App.java"), marker
}

func TestRealJavaNeverImportsTheBuild(t *testing.T) {
	t.Parallel()
	requireRealJdtls(t)

	dir := t.TempDir()
	app, marker := writeJavaProject(t, dir)
	before := listTree(t, dir)

	m := NewManager(dir, "", t.TempDir(), context.Background())
	t.Cleanup(m.Shutdown)

	got, err := m.Hover(app, javaGreetUseLine, javaGreetUseChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if !strings.Contains(got, "friendly greeting") {
		t.Errorf("hover = %q, want the Javadoc from Greeter.java", got)
	}
	got, err = m.Hover(app, javaPrintlnLine, javaPrintlnChar)
	if err != nil {
		t.Fatalf("Hover(println): %v", err)
	}
	if !strings.Contains(got, "java.io.PrintStream") {
		t.Errorf("hover(println) = %q, want java.io.PrintStream", got)
	}
	refs, err := m.References(app, javaGreetUseLine, javaGreetUseChar)
	if err != nil || len(refs) != 0 {
		t.Errorf("References = %+v, %v; want none and no error", refs, err)
	}
	m.Shutdown()

	if after := listTree(t, dir); !slices.Equal(after, before) {
		t.Errorf("repo files changed:\nbefore %v\nafter  %v", before, after)
	}
	if ran, err := os.ReadFile(marker); err == nil {
		t.Errorf("settings.gradle was evaluated: %q", ran)
	}
	data := filepath.Join(testSandbox(t, m).Workspace("java").Scratch, "data")
	if !isDir(filepath.Join(data, ".metadata")) {
		t.Errorf("workspace data dir %s was not used by the server", data)
	}
}
