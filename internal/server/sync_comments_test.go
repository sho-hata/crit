package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sho-hata/crit/internal/review"
)

// writeExternalCommentForTest overwrites the on-disk review file with a
// single comment, simulating an external process (e.g. a prior version of
// crit's share-pull merge) changing the file out from under a running
// session. SyncCommentsFromDisk tests use this to seed the "changed on
// disk" precondition without depending on any particular external writer.
func writeExternalCommentForTest(t *testing.T, critPath, filePath string, comment Comment) {
	t.Helper()
	if err := review.SaveCritJSON(critPath, CritJSON{
		Files: map[string]CritJSONFile{
			filePath: {Comments: []Comment{comment}},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSyncCommentsFromDisk_ClearsPendingWrite(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(filePath, []byte("# Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	critPath := filepath.Join(dir, ".crit")
	if err := review.SaveCritJSON(critPath, CritJSON{
		Files: map[string]CritJSONFile{
			"plan.md": {Comments: []Comment{}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	sess := &Session{
		Mode:      "files",
		OutputDir: dir,
		RepoRoot:  dir,
		Files:     []*FileEntry{{Path: "plan.md", AbsPath: filePath}},
	}
	// Scheduling a debounced write (any mutation does this) blocks mergeExternalCritJSON.
	if _, ok := sess.AddComment("plan.md", 1, 1, "", "local pending comment", "", "tester", ""); !ok {
		t.Fatal("AddComment failed")
	}
	if sess.PendingWriteForTest() != true {
		t.Fatal("expected pendingWrite after AddComment")
	}

	writeExternalCommentForTest(t, critPath, "plan.md", Comment{
		ID: "c_remote1", Body: "remote", StartLine: 1, EndLine: 1,
	})

	if !sess.SyncCommentsFromDisk() {
		t.Fatal("SyncCommentsFromDisk returned false with pendingWrite set")
	}
	if len(sess.GetComments("plan.md")) != 1 {
		t.Fatalf("want 1 comment, got %d", len(sess.GetComments("plan.md")))
	}
}

func TestSyncCommentsFromDisk_AfterMergeWebComments_WithNewServer(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(filePath, []byte("# Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sess := &Session{
		Mode:        "files",
		OutputDir:   dir,
		RepoRoot:    dir,
		ReviewRound: 1,
		Files:       []*FileEntry{{Path: "plan.md", AbsPath: filePath}},
	}
	writeCritJSONForTest(t, dir, CritJSON{
		Files: map[string]CritJSONFile{
			"plan.md": {Comments: []Comment{}},
		},
	})
	if info, err := os.Stat(review.ReviewPathsFor(sess.CritJSONPath()).Review); err != nil {
		t.Fatal(err)
	} else {
		sess.SetLastCritJSONMtimeForTest(info.ModTime())
	}

	if _, err := NewServer(sess, frontendFS, "", "test", 0, ""); err != nil {
		t.Fatal(err)
	}

	critPath := sess.CritJSONPath()
	writeExternalCommentForTest(t, critPath, "plan.md", Comment{
		ID: "ext-1", Body: "new web comment", StartLine: 1, EndLine: 1, Author: "Web User",
	})

	if !sess.SyncCommentsFromDisk() {
		t.Fatalf("SyncCommentsFromDisk returned false (pendingWrite=%v)", sess.PendingWriteForTest())
	}
	if len(sess.GetComments("plan.md")) != 1 {
		t.Fatalf("want 1 visible comment")
	}
}

func TestSyncCommentsFromDisk_AfterMergeWebComments(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(filePath, []byte("# Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	critPath := filepath.Join(dir, ".crit")
	if err := review.SaveCritJSON(critPath, CritJSON{
		Files: map[string]CritJSONFile{
			"plan.md": {Comments: []Comment{}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	sess := &Session{
		Mode:      "files",
		OutputDir: dir,
		RepoRoot:  dir,
		Files:     []*FileEntry{{Path: "plan.md", AbsPath: filePath}},
	}
	if info, err := os.Stat(review.ReviewPathsFor(critPath).Review); err != nil {
		t.Fatal(err)
	} else {
		sess.SetLastCritJSONMtimeForTest(info.ModTime())
	}

	writeExternalCommentForTest(t, critPath, "plan.md", Comment{
		ID: "ext-1", Body: "new web comment", StartLine: 1, EndLine: 1, Author: "Web User",
	})

	if !sess.SyncCommentsFromDisk() {
		t.Fatal("SyncCommentsFromDisk returned false")
	}

	sess.RLock()
	raw := len(sess.Files[0].Comments)
	focus := sess.Focus
	sess.RUnlock()
	if raw != 1 {
		t.Fatalf("in-memory file has %d comments, want 1", raw)
	}

	visible := sess.GetComments("plan.md")
	if len(visible) != 1 {
		t.Fatalf("GetComments returned %d, want 1 (raw=%d focus=%+v)", len(visible), raw, focus)
	}
}
