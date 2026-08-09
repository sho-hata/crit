package pathsafe

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUnder(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "file.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	escapeLink := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escapeLink); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		root    string
		wantErr error
	}{
		{"inside", inside, root, nil},
		{"root itself rejected", root, root, ErrDenied},
		{"missing file", filepath.Join(root, "nope.txt"), root, ErrNotFound},
		{"outside root", outside, root, ErrDenied},
		{"symlink escaping root", escapeLink, root, ErrDenied},
		{"empty root", inside, "", ErrDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := ResolveUnder(tt.path, tt.root)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && resolved == "" {
				t.Error("resolved path must be non-empty on success")
			}
		})
	}
}
