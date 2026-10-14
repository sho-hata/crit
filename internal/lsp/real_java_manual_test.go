package lsp

// Opt-in integration tests against a real jdt.ls (same CRIT_LSP_REAL gate as
// the other real-server tests). Skipped unless enabled:
//
//	CRIT_LSP_REAL=1 go test ./internal/lsp -run TestRealJava -v
//
// Requires jdtls (Eclipse JDT Language Server's launcher) and a JDK 21+ on
// PATH. No Gradle or Maven: the syntax server never runs a build tool.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

// Positions (0-based line, UTF-16 character) in javaAppSource.
const (
	javaGreetUseLine, javaGreetUseChar = 4, 35 // greet
	javaGreetDefLine                   = 5     // "public static String greet" in javaGreeterSource
	javaPrintlnLine, javaPrintlnChar   = 4, 19 // println
)

func requireRealJdtls(t *testing.T) {
	t.Helper()
	if os.Getenv("CRIT_LSP_REAL") == "" {
		t.Skip("set CRIT_LSP_REAL=1 to run against a real jdt.ls")
	}
	if !LanguageForPath("x.java").Available() {
		t.Fatal("jdtls not on PATH")
	}
}

// writeJavaProject lays out the two sources plus a Gradle build whose settings
// script records that it was evaluated — what a branch would commit to run
// code on import. Returns App.java's path and the marker the script writes.
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

// The standard jdt.ls imports a Gradle project by evaluating its build
// scripts (measured with 1.61.0: it started Gradle within 40s of didOpen and
// wrote .gradle/, .project and .settings/ into the fixture). The syntax
// server crit runs must do none of that, and still answer from the sources.
func TestRealJavaNeverImportsTheBuild(t *testing.T) {
	t.Parallel()
	requireRealJdtls(t)

	dir := t.TempDir()
	app, marker := writeJavaProject(t, dir)
	before := listTree(t, dir)

	m := NewManager(dir, "", context.Background())
	t.Cleanup(m.Shutdown) // a failed Hover must not leak the server

	got, err := m.Hover(app, javaGreetUseLine, javaGreetUseChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if !strings.Contains(got, "friendly greeting") {
		t.Errorf("hover = %q, want the Javadoc from Greeter.java", got)
	}
	// JDK types resolve from the running JDK, no build needed.
	got, err = m.Hover(app, javaPrintlnLine, javaPrintlnChar)
	if err != nil {
		t.Fatalf("Hover(println): %v", err)
	}
	if !strings.Contains(got, "java.io.PrintStream") {
		t.Errorf("hover(println) = %q, want java.io.PrintStream", got)
	}
	// The syntax server declares no references provider: empty, not an
	// error the UI would count toward its failure breaker.
	refs, err := m.References(app, javaGreetUseLine, javaGreetUseChar)
	if err != nil || len(refs) != 0 {
		t.Errorf("References = %+v, %v; want none and no error", refs, err)
	}
	// Stop it before listing: a still-running server could write afterwards.
	m.Shutdown()

	if after := listTree(t, dir); !slices.Equal(after, before) {
		t.Errorf("repo files changed:\nbefore %v\nafter  %v", before, after)
	}
	if ran, err := os.ReadFile(marker); err == nil {
		t.Errorf("settings.gradle was evaluated: %q", ran)
	}
	// Its Eclipse workspace lives under ~/.crit, not in the repo.
	data := jdtlsDataDir(dir)
	if strings.HasPrefix(data, dir+string(filepath.Separator)) {
		t.Errorf("workspace data dir %s is inside the repo", data)
	}
	if !isDir(filepath.Join(data, ".metadata")) {
		t.Errorf("workspace data dir %s was not used by the server", data)
	}
}
