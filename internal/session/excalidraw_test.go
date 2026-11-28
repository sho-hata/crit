package session

import "testing"

func TestIsExcalidrawScene(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]bool{
		"arch.excalidraw":         true,
		"docs/Arch.EXCALIDRAW":    true,
		"arch.excalidraw.json":    true,
		"arch.Excalidraw.JSON":    true,
		"arch.json":               false,
		"excalidraw.html":         false,
		"arch.excalidraw.md":      false,
		"notes/excalidraw/a.html": false,
	} {
		if got := IsExcalidrawScene(path); got != want {
			t.Errorf("IsExcalidrawScene(%q) = %v, want %v", path, got, want)
		}
	}
}
