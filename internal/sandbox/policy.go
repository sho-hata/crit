package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// policy is the one description of what a confined process may do. Each
// backend renders it and adds nothing of its own.
type policy struct {
	readDeny    []string // secrets: unreadable, even inside a tree
	trees       []string // Root, then DepRoot if any: readable, never executed or mapped executable
	scratchBase string   // every server's scratch: never executed or mapped executable
	scratch     string   // the only writable directory (besides /dev/null and friends)
	env         []string
	argv        []string // argv[0] absolute, outside the trees
	dir         string
}

func plan(t Trees, scratchBase, scratch, dir string, p Process, readDeny, environ []string) (policy, error) {
	env, err := buildEnv(environ, t, scratch, p)
	if err != nil {
		return policy{}, err
	}
	var dirs []string
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); strings.EqualFold(k, "PATH") {
			dirs = filepath.SplitList(v)
		}
	}
	bin, err := resolveArgv0(p.Argv, dirs, t.list())
	if err != nil {
		return policy{}, err
	}
	// The whole scratch base, not just this process's scratch: a file one
	// server wrote into its own scratch must not run from another's.
	pol := policy{
		scratchBase: realPath(scratchBase),
		scratch:     realPath(scratch),
		env:         env,
		argv:        append([]string{bin}, p.Argv[1:]...),
		dir:         dir,
	}
	// Every entry, whether or not it exists yet: a secret created after the
	// spawn (the first `aws configure`) must stay unreadable to a server
	// that lives for hours.
	for _, s := range readDeny {
		pol.readDeny = append(pol.readDeny, realPath(s))
	}
	for _, tree := range t.list() {
		pol.trees = append(pol.trees, realPath(tree))
	}
	return pol, nil
}

// denied is the read-deny set: the secrets, plus crit's own reviews
// directory (two levels above the scratch base) so a server cannot read
// other reviews' comments. The reviews directory is left out when denying
// it would take the filesystem root or the reviewer's home with it, or when
// a tree lies beneath it; the trees and the scratch are re-allowed anyway.
func denied(home, scratchBase string, t Trees, getenv func(string) string) []string {
	out := secrets(home, getenv)
	reviews := filepath.Dir(filepath.Dir(scratchBase))
	// Under range focus Root is a worktree inside the reviews directory and
	// readAllow re-opens it, so only the working tree decides whether the
	// reviews directory as a whole stays readable.
	working := t.Root
	if t.DepRoot != "" {
		working = t.DepRoot
	}
	if reviews == filepath.Dir(reviews) || Beneath(home, reviews) || Beneath(working, reviews) {
		return out
	}
	return append(out, reviews)
}

// secrets lists what a confined process must not read, relative to home:
// credential stores of the shipped toolchains (git, terraform, Composer,
// npm, pip, bundler), cloud and cluster logins, agent and shell state,
// and crit's own data. The tree under review and the scratch live under
// ~/.crit, so readAllow re-opens them. Shell startup files are in: exported
// tokens live there. A store moved by its environment variable is denied
// where the variable points as well as at its default.
func secrets(home string, getenv func(string) string) []string {
	rel := []string{
		".ssh", ".aws", ".gnupg", ".netrc", ".npmrc", ".pypirc", ".kube", ".docker",
		".config/gh", ".config/gcloud", ".azure",
		".cargo/credentials", ".cargo/credentials.toml", ".gem/credentials",
		".m2/settings.xml", ".gradle/gradle.properties",
		".git-credentials", ".config/git/credentials",
		".terraform.d/credentials.tfrc.json", ".terraformrc",
		".composer/auth.json", ".config/composer/auth.json",
		".config/pip", ".pip",
		".bundle/config", ".pgpass", ".vault-token", ".password-store",
		".local/share/keyrings", ".config/op", ".config/hub",
		".claude", ".claude.json", ".codex", ".zsh_history", ".bash_history",
		".zshrc", ".zshenv", ".zprofile", ".zlogin", ".bashrc", ".bash_profile", ".profile",
		".yarnrc", ".yarnrc.yml",
		".crit", ".crit.config.json",
		"Library/Keychains", "Library/Application Support/pip",
	}
	out := make([]string, 0, len(rel))
	for _, r := range rel {
		out = append(out, filepath.Join(home, filepath.FromSlash(r)))
	}
	for _, m := range moved {
		for _, base := range filepath.SplitList(getenv(m.env)) {
			// Home or above it would deny the toolchains too; a relative
			// value names nothing the profile can match.
			if !filepath.IsAbs(base) || Beneath(home, base) {
				continue
			}
			if m.rel == nil {
				out = append(out, filepath.Clean(base))
			}
			for _, r := range m.rel {
				out = append(out, filepath.Join(base, filepath.FromSlash(r)))
			}
		}
	}
	return out
}

// moved is where an environment variable relocates a store secrets lists:
// the value itself, or rel beneath it. KUBECONFIG is a path list, the rest
// split into one.
var moved = []struct {
	env string
	rel []string
}{
	{"XDG_CONFIG_HOME", []string{"gh", "gcloud", "git/credentials", "composer/auth.json", "pip", "op", "hub"}},
	{"XDG_DATA_HOME", []string{"keyrings"}},
	{"ZDOTDIR", []string{".zshrc", ".zshenv", ".zprofile", ".zlogin", ".zsh_history"}},
	{"CARGO_HOME", []string{"credentials", "credentials.toml"}},
	{"COMPOSER_HOME", []string{"auth.json"}},
	{"AWS_SHARED_CREDENTIALS_FILE", nil}, {"AWS_CONFIG_FILE", nil},
	{"GOOGLE_APPLICATION_CREDENTIALS", nil}, {"CLOUDSDK_CONFIG", nil}, {"AZURE_CONFIG_DIR", nil},
	{"KUBECONFIG", nil}, {"DOCKER_CONFIG", nil}, {"GNUPGHOME", nil}, {"PASSWORD_STORE_DIR", nil},
	{"NETRC", nil}, {"NPM_CONFIG_USERCONFIG", nil}, {"PIP_CONFIG_FILE", nil},
	{"BUNDLE_USER_CONFIG", nil}, {"TF_CLI_CONFIG_FILE", nil}, {"PGPASSFILE", nil},
	{"CLAUDE_CONFIG_DIR", nil}, {"CODEX_HOME", nil}, {"HISTFILE", nil},
}

// envAllow is every variable a confined process inherits from crit, by exact
// name: locale and identity, toolchain locators, TLS roots. The Windows
// entries are what node, python and .cmd shims need to start at all; they
// are absent elsewhere. GOPROXY is left out: its URL can carry credentials
// (https://user:token@proxy) and the jail has no network to use it on.
var envAllow = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "TZ",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES",
	"GOROOT", "GOPATH", "GOMODCACHE", "GOFLAGS", "GONOPROXY", "GONOSUMDB", "GOPRIVATE", "GOWORK",
	"JAVA_HOME", "NODE_PATH", "NVM_DIR", "VIRTUAL_ENV", "CONDA_PREFIX", "PYENV_ROOT",
	"CARGO_HOME", "RUSTUP_HOME", "RUSTUP_TOOLCHAIN", "GEM_HOME", "GEM_PATH", "RBENV_ROOT",
	"PUB_CACHE", "FLUTTER_ROOT", "SDKMAN_DIR", "DOTNET_ROOT", "PERL5LIB",
	"MISE_DATA_DIR", "MISE_CONFIG_DIR", "ASDF_DIR", "ASDF_DATA_DIR", "HOMEBREW_PREFIX",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "USERPROFILE", "USERNAME",
	"HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES",
}

// envReserved is what Process.Env may not override: the jail owns them.
var envReserved = []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP"}

// buildEnv is the allowlist, PATH without relative or in-tree entries, the
// temp variables and XDG_CACHE_HOME pointed into scratch, then Process.Env.
// Everything else is dropped. macOS ignores XDG_CACHE_HOME (os.UserCacheDir
// is ~/Library/Caches there, unwritable), so a program that caches says where
// through Process.Env (gopls: GOCACHE, GOPLSCACHE).
func buildEnv(environ []string, t Trees, scratch string, p Process) ([]string, error) {
	for _, kv := range p.Env {
		if k, _, _ := strings.Cut(kv, "="); slices.ContainsFunc(envReserved, func(r string) bool { return strings.EqualFold(r, k) }) {
			return nil, fmt.Errorf("sandbox: Env may not set %s", k)
		}
	}
	var env []string
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !slices.ContainsFunc(envAllow, func(a string) bool { return strings.EqualFold(a, k) }) {
			continue
		}
		if strings.EqualFold(k, "PATH") {
			v = scrubPath(v, t.list())
		}
		env = append(env, k+"="+v)
	}
	tmp := filepath.Join(scratch, "tmp")
	env = append(env,
		"TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp,
		"XDG_CACHE_HOME="+filepath.Join(scratch, "cache"),
		"GOTOOLCHAIN=local",
	)
	return append(env, p.Env...), nil
}

// scrubPath drops relative entries and entries under a tree, so a bare
// program name can never resolve into the code under review.
func scrubPath(path string, trees []string) string {
	var keep []string
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		dir = filepath.Clean(dir)
		if slices.ContainsFunc(trees, func(tree string) bool { return Beneath(dir, tree) }) {
			continue
		}
		keep = append(keep, dir)
	}
	return strings.Join(keep, string(os.PathListSeparator))
}
