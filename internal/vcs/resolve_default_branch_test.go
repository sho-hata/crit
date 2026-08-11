package vcs

import (
	"testing"
)

func TestResolveDefaultBranchSHA_SaplingUnsupported(t *testing.T) {
	t.Parallel()

	_, err := ResolveDefaultBranchSHA(&SaplingVCS{}, t.TempDir(), "main")
	if err == nil {
		t.Fatal("expected error without sl binary")
	}
}

func TestResolveDefaultBranchSHA_JJUnsupported(t *testing.T) {
	t.Parallel()

	_, err := ResolveDefaultBranchSHA(&JJVCS{}, t.TempDir(), "main")
	if err == nil {
		t.Fatal("expected error without jj binary")
	}
}
