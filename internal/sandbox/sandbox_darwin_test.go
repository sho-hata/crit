package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func catUnderJail(t *testing.T, j *Jail, path string) (string, error) {
	t.Helper()
	cmd, err := j.Command(context.Background(), "t", Process{Argv: []string{"/bin/cat", path}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

// Credentials moved out of $HOME by an environment variable are as
// unreadable as at their default place; the control reads them plainly.
func TestJailDeniesRelocatedSecrets(t *testing.T) {
	requireBackend(t)
	t.Setenv("HOME", realTempDir(t))
	xdg, cargo := realTempDir(t), realTempDir(t)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("CARGO_HOME", cargo)
	secrets := []string{filepath.Join(xdg, "gh", "hosts.yml"), filepath.Join(cargo, "credentials.toml")}
	for _, p := range secrets {
		writeFile(t, p, "token\n", 0o600)
	}
	other := filepath.Join(xdg, "nvim", "init.lua")
	writeFile(t, other, "ok\n", 0o600)
	j, err := Open(Trees{Root: realTempDir(t)}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range secrets {
		if out, err := catUnderJail(t, j, p); err == nil {
			t.Errorf("%s readable under the jail: %q", p, out)
		}
	}
	if out, err := catUnderJail(t, j, other); err != nil {
		t.Errorf("the rest of XDG_CONFIG_HOME must stay readable: %v: %s", err, out)
	}
}

// Reviewing a directory the secrets list names must not blind the server
// to the tree it runs on; a secret beneath it stays unreadable.
func TestJailReadsATreeThatIsASecret(t *testing.T) {
	requireBackend(t)
	home := realTempDir(t)
	t.Setenv("HOME", home)
	tree := filepath.Join(home, ".password-store")
	writeFile(t, filepath.Join(tree, "a.txt"), "hello\n", 0o600)
	t.Setenv("GNUPGHOME", filepath.Join(tree, "gnupg"))
	key := filepath.Join(tree, "gnupg", "key")
	writeFile(t, key, "key\n", 0o600)
	j, err := Open(Trees{Root: tree}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := catUnderJail(t, j, filepath.Join(tree, "a.txt")); err != nil || out != "hello\n" {
		t.Errorf("tree file: %v %q", err, out)
	}
	if out, err := catUnderJail(t, j, key); err == nil {
		t.Errorf("secret beneath the tree readable: %q", out)
	}
}

// The Go-side checks compare paths as spelled; on a case-insensitive volume
// the same tree has other spellings. The kernel matches the file, not the
// spelling, so a tree file does not run whichever way either is written.
func TestJailRefusesTreeExecUnderAnotherSpelling(t *testing.T) {
	requireBackend(t)
	t.Setenv("HOME", realTempDir(t))
	base := realTempDir(t)
	tree, other := filepath.Join(base, "Repo"), filepath.Join(base, "REPO")
	writeFile(t, filepath.Join(tree, "evil"), "#!/bin/sh\necho ran\n", 0o755)
	if _, err := os.Stat(filepath.Join(other, "evil")); err != nil {
		t.Skip("case-sensitive volume")
	}
	for _, root := range []string{tree, other} {
		j, err := Open(Trees{Root: root}, realTempDir(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, bin := range []string{filepath.Join(tree, "evil"), filepath.Join(other, "evil")} {
			cmd, err := j.Command(context.Background(), "t", Process{Argv: []string{"/bin/sh", "-c", `"$0"`, bin}})
			if err != nil {
				t.Fatal(err)
			}
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Errorf("root %s: %s ran under the jail (%q)", root, bin, out)
			}
		}
	}
}

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
		cmd, err := j.Command(context.Background(), "t", Process{Argv: []string{loader, lib}})
		if err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%s loaded under the jail (%q)", lib, out)
		}
	}
}
