package focus

import (
	"testing"

	"github.com/sho-hata/crit/internal/vcs"
)

func TestResolveFocusFromPR(t *testing.T) {
	prevFetch := FetchPRHook
	prevStack := IsStackedPRHook
	t.Cleanup(func() {
		FetchPRHook = prevFetch
		IsStackedPRHook = prevStack
	})

	IsStackedPRHook = func(PRResolveInfo, vcs.VCS) bool { return false }

	dir := vcs.InitTestRepo(t)
	v := &vcs.GitVCS{}
	// Seed objects locally so EnsureSHAFetched short-circuits.
	base := vcs.GitRun(t, dir, "rev-parse", "HEAD")
	head := vcs.CommitAtForTest(t, dir, "pr.txt", "x", "pr change")

	FetchPRHook = func(spec string) (PRResolveInfo, error) {
		return PRResolveInfo{
			Number:      42,
			Title:       "Test PR",
			BaseRefOid:  base,
			HeadRefOid:  head,
			BaseRefName: "main",
			HeadRefName: "feature",
		}, nil
	}

	// remoteFiles skips EnsureSHAFetched; this test wires PR hooks, not git fetch.
	f, err := ResolveFocus("42", "", "", true, v, dir)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil || f.PRNumber != 42 || f.HeadSHA != head {
		t.Errorf("got %+v", f)
	}
}

func TestSetPRResolveHooks(t *testing.T) {
	t.Parallel()

	SetPRResolveHooks(
		func(string) (PRResolveInfo, error) { return PRResolveInfo{Number: 1}, nil },
		func(PRResolveInfo, vcs.VCS) bool { return true },
	)
	if FetchPRHook == nil || IsStackedPRHook == nil {
		t.Fatal("hooks not wired")
	}
}
