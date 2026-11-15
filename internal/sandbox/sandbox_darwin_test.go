package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The canary's exec probes are shebang scripts and never map native code, so
// this one covers file-map-executable: a dylib from the tree or the scratch
// does not load, while a loader outside both still runs and loads it
// unconfined (the control).
func TestJailRefusesNativeCodeFromTheTrees(t *testing.T) {
	requireBackend(t)
	cc, err := exec.LookPath("cc")
	if err != nil {
		if os.Getenv("CRIT_REQUIRE_SANDBOX") != "" {
			t.Fatal("CRIT_REQUIRE_SANDBOX is set and there is no cc to build the probe")
		}
		t.Skip("no cc")
	}
	src := t.TempDir()
	tools := realTempDir(t)
	tree := realTempDir(t)
	j, err := Open(Trees{Root: tree}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	build := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(cc, args...).CombinedOutput(); err != nil { //nolint:gosec // cc from PATH builds the probe
			t.Fatalf("cc %v: %v: %s", args, err, out)
		}
	}
	write(filepath.Join(src, "lib.c"), "int canary(void) { return 42; }\n")
	write(filepath.Join(src, "load.c"), `#include <dlfcn.h>
#include <stdio.h>
int main(int argc, char **argv) {
	if (!dlopen(argv[1], RTLD_NOW)) { fprintf(stderr, "%s\n", dlerror()); return 1; }
	return 0;
}
`)
	loader := filepath.Join(tools, "load")
	build("-o", loader, filepath.Join(src, "load.c"))
	treeLib := filepath.Join(tree, "lib.dylib")
	build("-dynamiclib", "-o", treeLib, filepath.Join(src, "lib.c"))

	ws := j.Workspace("t")
	scratchLib := filepath.Join(ws.Scratch, "lib.dylib")
	if err := os.MkdirAll(ws.Scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(treeLib)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scratchLib, data, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, lib := range []string{treeLib, scratchLib} {
		if out, err := exec.Command(loader, lib).CombinedOutput(); err != nil { //nolint:gosec // control: the probe loads unconfined
			t.Fatalf("control: %s does not load unconfined: %v: %s", lib, err, out)
		}
		cmd, err := j.Command(context.Background(), "t", func(Workspace) Process {
			return Process{Argv: []string{loader, lib}}
		})
		if err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%s loaded under the jail (%q)", lib, out)
		}
	}
}
