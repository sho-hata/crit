package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sho-hata/crit/internal/focus"
	"github.com/sho-hata/crit/internal/github"
	"github.com/sho-hata/crit/internal/session"
	"github.com/sho-hata/crit/internal/testutil"
)

func TestWire_ResolveServerConfigPassesSessionID(t *testing.T) {
	dir := t.TempDir()
	homeDir := t.TempDir()
	testutil.SetHome(t, homeDir)

	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	sc, err := session.ResolveServerConfigFn([]string{"--session", "839f3b4cd5d6", "--no-open"})
	if err != nil {
		t.Fatalf("ResolveServerConfigFn: %v", err)
	}
	if sc.SessionID != "839f3b4cd5d6" {
		t.Errorf("SessionID = %q", sc.SessionID)
	}
}

func TestWirePRResolveHooks_ParsesSpecAndPropagatesErrors(t *testing.T) {
	prevFetch, prevStacked := focus.FetchPRHook, focus.IsStackedPRHook
	t.Cleanup(func() { focus.SetPRResolveHooks(prevFetch, prevStacked) })

	restore := github.SwapFetchPRByNumberForTest(func(n int) (*github.PRInfo, error) {
		if n == 99 {
			return nil, errors.New("fetch boom")
		}
		return &github.PRInfo{
			URL:               "https://github.com/myorg/repo-b/pull/1",
			Number:            n,
			Title:             "Cross-repo",
			BaseRefOid:        "base",
			HeadRefOid:        "head",
			BaseRefName:       "main",
			HeadRefName:       "feat",
			HeadRepoURL:       "https://github.com/fork/repo-b.git",
			IsCrossRepository: true,
		}, nil
	})
	t.Cleanup(restore)

	wirePRResolveHooks()

	info, err := focus.FetchPRHook("https://github.com/myorg/repo-b/pull/1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Number != 1 || info.HeadRepoURL != "https://github.com/fork/repo-b.git" || !info.IsCrossRepository {
		t.Errorf("info = %+v", info)
	}

	if _, err := focus.FetchPRHook("7"); err != nil {
		t.Fatal(err)
	}
	if _, err := focus.FetchPRHook("not-a-pr"); err == nil {
		t.Fatal("expected parse error")
	}
	if _, err := focus.FetchPRHook("99"); err == nil || !strings.Contains(err.Error(), "fetch boom") {
		t.Fatalf("fetch error = %v", err)
	}
	if focus.IsStackedPRHook == nil || focus.IsStackedPRHook(info, nil) {
		t.Fatal("IsStackedPRHook(nil vcs) should be false")
	}
}
