package session

import "strings"

// IsExcalidrawScene reports whether path names an Excalidraw scene
// (*.excalidraw or *.excalidraw.json, any case), which preview mode opens as
// a canvas.
func IsExcalidrawScene(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".excalidraw") || strings.HasSuffix(lower, ".excalidraw.json")
}
