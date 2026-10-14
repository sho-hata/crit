package lsp

import (
	"path/filepath"
	"strings"
	"testing"
)

// Not parallel: it sets HOME so the workspace dirs land in a temp dir.
func TestJdtlsDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	a := jdtlsDataDir(filepath.Join(home, "work", "repo"))
	// Two roots with the same base name — the launcher's own key — must not
	// share a workspace, or the second server cannot start.
	b := jdtlsDataDir(filepath.Join(home, "other", "repo"))
	if a == b {
		t.Errorf("roots with the same base name share the data dir %s", a)
	}
	if again := jdtlsDataDir(filepath.Join(home, "work", "repo")); again != a {
		t.Errorf("same root gave %s then %s, want a stable dir", a, again)
	}
	// A trailing separator is the same root.
	if slash := jdtlsDataDir(filepath.Join(home, "work", "repo") + string(filepath.Separator)); slash != a {
		t.Errorf("root with a trailing separator gave %s, want %s", slash, a)
	}

	want := filepath.Join(home, ".crit", "lsp", "jdtls")
	if !strings.HasPrefix(a, want+string(filepath.Separator)) {
		t.Errorf("data dir %s is not under %s", a, want)
	}
	if !isDir(a) {
		t.Errorf("data dir %s was not created", a)
	}

	args := jdtlsArgs(filepath.Join(home, "work", "repo"))
	if len(args) != 2 || args[0] != "-data" || args[1] != a {
		t.Errorf("jdtlsArgs = %v, want [-data %s]", args, a)
	}
}
