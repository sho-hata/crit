package lsp

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestJdtlsArgsKeepTheWorkspaceInTheScratch(t *testing.T) {
	t.Parallel()

	ws := Workspace{Root: "/repo", Scratch: "/scratch/java/abc"}
	args := jdtlsArgs(ws)
	for _, flag := range []string{"-data", "-configuration"} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("jdtlsArgs = %v, want %s <dir>", args, flag)
		}
		dir := args[i+1]
		if !strings.HasPrefix(dir, ws.Scratch+string(filepath.Separator)) {
			t.Errorf("%s %s is not under the scratch %s", flag, dir, ws.Scratch)
		}
		if strings.HasPrefix(dir, filepath.Join(ws.Scratch, "tmp")) {
			t.Errorf("%s %s lies under tmp/, which every start empties", flag, dir)
		}
	}
}
