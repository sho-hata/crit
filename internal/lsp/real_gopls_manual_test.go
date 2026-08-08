package lsp

// Opt-in integration test against a real gopls (mirrors the env-gated
// pattern of the GitHub roundtrip suite). Skipped unless enabled:
//
//	CRIT_LSP_REAL=1 go test ./internal/lsp -run TestRealGopls -v
//
// Requires gopls AND the go toolchain on PATH — gopls cannot build a
// workspace view without a `go` binary and fails with "no views".

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRealGopls(t *testing.T) {
	if os.Getenv("CRIT_LSP_REAL") == "" {
		t.Skip("set CRIT_LSP_REAL=1 to run against a real gopls")
	}
	if !GoplsAvailable() {
		t.Fatal("gopls not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module smoke\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "main.go")
	src := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"
	if err := os.WriteFile(main, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(dir, context.Background())
	defer m.Shutdown()

	// Hover over "Println" (line 6 → 0-based 5; "\tfmt.Println" → char 5)
	got, err := m.Hover(main, 5, 6)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	t.Logf("hover result: %q", got)
	if got == "" {
		t.Error("empty hover for fmt.Println")
	}
}
