package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func catUnderSandbox(t *testing.T, sb *Sandbox, path string) (string, error) {
	t.Helper()
	cmd, err := sb.Command(context.Background(), "t", Process{Argv: []string{"/bin/cat", path}})
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

func TestSandboxDeniesRelocatedSecrets(t *testing.T) {
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
	sb, err := New(Trees{Root: realTempDir(t)}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range secrets {
		if out, err := catUnderSandbox(t, sb, p); err == nil {
			t.Errorf("%s readable under the sandbox: %q", p, out)
		}
	}
	if out, err := catUnderSandbox(t, sb, other); err != nil {
		t.Errorf("the rest of XDG_CONFIG_HOME must stay readable: %v: %s", err, out)
	}
}

func TestSandboxReadsATreeThatIsASecret(t *testing.T) {
	requireBackend(t)
	home := realTempDir(t)
	t.Setenv("HOME", home)
	tree := filepath.Join(home, ".password-store")
	writeFile(t, filepath.Join(tree, "a.txt"), "hello\n", 0o600)
	t.Setenv("GNUPGHOME", filepath.Join(tree, "gnupg"))
	key := filepath.Join(tree, "gnupg", "key")
	writeFile(t, key, "key\n", 0o600)
	sb, err := New(Trees{Root: tree}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := catUnderSandbox(t, sb, filepath.Join(tree, "a.txt")); err != nil || out != "hello\n" {
		t.Errorf("tree file: %v %q", err, out)
	}
	if out, err := catUnderSandbox(t, sb, key); err == nil {
		t.Errorf("secret beneath the tree readable: %q", out)
	}
}

// A secret behind a symlink whose target appears after the policy is built
// (a volume mounted later) is unreadable at the target as well as the link.
func TestSandboxDeniesASecretBehindADanglingSymlink(t *testing.T) {
	requireBackend(t)
	home := realTempDir(t)
	t.Setenv("HOME", home)
	vault := filepath.Join(realTempDir(t), "vault")
	if err := os.Symlink(filepath.Join(vault, "ssh"), filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	sb, err := New(Trees{Root: realTempDir(t)}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(vault, "ssh", "id"), filepath.Join(home, ".ssh", "id")} {
		cmd, err := sb.Command(context.Background(), "t", Process{Argv: []string{"/bin/cat", p}})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(vault, "ssh", "id"), "key\n", 0o600)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%s readable under the sandbox: %q", p, out)
		}
		if err := os.RemoveAll(vault); err != nil {
			t.Fatal(err)
		}
	}
}

// The reviewer's git config is unreadable, and git still runs without it.
func TestSandboxHidesGitConfig(t *testing.T) {
	requireBackend(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	home := realTempDir(t)
	t.Setenv("HOME", home)
	config := filepath.Join(home, ".gitconfig")
	writeFile(t, config, "[http]\n\textraheader = Authorization: token x\n", 0o600)
	if out, err := exec.Command(git, "config", "--global", "--list").CombinedOutput(); err != nil || !strings.Contains(string(out), "token x") { //nolint:gosec // control: git reads the config unconfined
		t.Fatalf("control: git does not read %s unconfined: %v: %s", config, err, out)
	}
	sb, err := New(Trees{Root: realTempDir(t)}, realTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := catUnderSandbox(t, sb, config); err == nil {
		t.Errorf("%s readable under the sandbox: %q", config, out)
	}
	cmd, err := sb.Command(context.Background(), "t", Process{Argv: []string{"git", "config", "--global", "--list"}})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Errorf("git under the sandbox: %v: %q, want it to run and see no config", err, out)
	}
}

// The Go-side checks compare paths as spelled; on a case-insensitive volume
// the same tree has other spellings. The kernel matches the file, not the
// spelling, so a tree file does not run whichever way either is written.
func TestSandboxRefusesTreeExecUnderAnotherSpelling(t *testing.T) {
	requireBackend(t)
	t.Setenv("HOME", realTempDir(t))
	base := realTempDir(t)
	tree, other := filepath.Join(base, "Repo"), filepath.Join(base, "REPO")
	writeFile(t, filepath.Join(tree, "evil"), "#!/bin/sh\necho ran\n", 0o755)
	if _, err := os.Stat(filepath.Join(other, "evil")); err != nil {
		t.Skip("case-sensitive volume")
	}
	for _, root := range []string{tree, other} {
		sb, err := New(Trees{Root: root}, realTempDir(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, bin := range []string{filepath.Join(tree, "evil"), filepath.Join(other, "evil")} {
			cmd, err := sb.Command(context.Background(), "t", Process{Argv: []string{"/bin/sh", "-c", `"$0"`, bin}})
			if err != nil {
				t.Fatal(err)
			}
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Errorf("root %s: %s ran under the sandbox (%q)", root, bin, out)
			}
		}
	}
}

// The canary's exec probes are shebang scripts and never map native code, so
// this one covers file-map-executable.
func TestSandboxRefusesNativeCodeFromTheTrees(t *testing.T) {
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
	sb, err := New(Trees{Root: tree}, realTempDir(t))
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

	ws := sb.Workspace("t")
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
		cmd, err := sb.Command(context.Background(), "t", Process{Argv: []string{loader, lib}})
		if err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%s loaded under the sandbox (%q)", lib, out)
		}
	}
}
