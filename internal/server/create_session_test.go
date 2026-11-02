package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sho-hata/crit/internal/session"
	"github.com/sho-hata/crit/internal/vcs"
)

func chdirRepo(t *testing.T, dir string) {
	t.Helper()
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func TestCreateSession_RangeFocus_CleanWorkingTree(t *testing.T) {
	tests := []struct {
		name           string
		featureBranch  bool
		wantBranch     string
		wantBaseBranch string
	}{
		{name: "default branch, clean tree", featureBranch: false, wantBranch: "main", wantBaseBranch: "main"},
		{name: "feature branch, clean tree", featureBranch: true, wantBranch: "feature/clean-tree", wantBaseBranch: "main"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := vcs.InitTestRepo(t)
			baseSHA := vcs.GitRun(t, dir, "rev-parse", "HEAD")
			if tc.featureBranch {
				vcs.GitRun(t, dir, "checkout", "-b", "feature/clean-tree")
			}
			if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			vcs.GitRun(t, dir, "add", "a.md")
			vcs.GitRun(t, dir, "commit", "-m", "add a")
			headSHA := vcs.GitRun(t, dir, "rev-parse", "HEAD")

			chdirRepo(t, dir)
			sess, err := CreateSession(&DaemonCLIConfig{
				Focus: &Focus{Kind: FocusRange, BaseSHA: baseSHA, HeadSHA: headSHA},
			})
			if err != nil {
				t.Fatalf("createSession with range focus on clean working tree: %v", err)
			}
			ApplySessionOverrides(sess, &DaemonCLIConfig{
				Focus: &Focus{Kind: FocusRange, BaseSHA: baseSHA, HeadSHA: headSHA},
			})
			if sess.Mode != "git" {
				t.Errorf("Mode = %q, want git", sess.Mode)
			}
			if sess.Branch != tc.wantBranch {
				t.Errorf("Branch = %q, want %q", sess.Branch, tc.wantBranch)
			}
			if sess.BaseBranchName != tc.wantBaseBranch {
				t.Errorf("BaseBranchName = %q, want %q", sess.BaseBranchName, tc.wantBaseBranch)
			}
		})
	}
}

func TestCreateSession_NoFocus_CleanWorkingTree(t *testing.T) {
	dir := vcs.InitTestRepo(t)
	vcs.GitRun(t, dir, "checkout", "-b", "feature/no-focus-clean")
	chdirRepo(t, dir)

	_, err := CreateSession(&DaemonCLIConfig{})
	if !errors.Is(err, session.ErrNoChangedFiles) {
		t.Fatalf("err = %v, want ErrNoChangedFiles", err)
	}
}

func TestApplySessionOverrides_OutputDirSkipsPlan(t *testing.T) {
	dir := vcs.InitTestRepo(t)
	chdirRepo(t, dir)
	sess, err := CreateSession(&DaemonCLIConfig{Files: []string{"README.md"}})
	if err != nil {
		t.Fatal(err)
	}
	beforeMode := sess.Mode
	ApplySessionOverrides(sess, &DaemonCLIConfig{
		PlanDir:   filepath.Join(t.TempDir(), "plans", "auth"),
		PlanName:  "auth",
		OutputDir: t.TempDir(),
	})
	if sess.Mode != beforeMode {
		t.Fatalf("OutputDir should skip plan overrides; Mode changed %q → %q", beforeMode, sess.Mode)
	}
	if sess.OutputDir != "" {
		t.Fatalf("OutputDir override must not set sess.OutputDir (legacy layout), got %q", sess.OutputDir)
	}
}
