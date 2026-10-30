package lsp

import (
	"runtime"
	"strings"
)

// serverEnvKeys is what a language server process inherits from the daemon:
// enough to find and run its toolchain (paths, locale, temp dir), to reach a
// module proxy through the reviewer's proxy and CA settings, and the
// toolchain's own configuration. Nothing else. The daemon runs in the
// reviewer's shell — GITHUB_TOKEN, cloud credentials, API keys — and a
// language server is a program the reviewed tree configures (tsconfig
// plugins, a go.mod toolchain line, cargo build scripts), so the environment
// is an allowlist, not the shell's minus a denylist.
var serverEnvKeys = []string{
	// Locating and running the toolchain.
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "TZ", "TERM",
	"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
	"NO_COLOR",
	// Windows: the process cannot start, or node/go cannot find their
	// caches, without these.
	"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT",
	"APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES", "ALLUSERSPROFILE",
	"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "USERNAME",
	// Reaching a module proxy / registry the reviewer's way.
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	// Go: where the toolchain, caches and proxies are. GOTOOLCHAIN is in
	// the list so a reviewer's explicit setting is visible, but the Go
	// language's Env pins it to local regardless.
	"GOROOT", "GOPATH", "GOBIN", "GOMODCACHE", "GOCACHE", "GOTMPDIR", "GOENV", "GOWORK",
	"GOFLAGS", "GOPROXY", "GONOPROXY", "GOSUMDB", "GONOSUMDB", "GOPRIVATE", "GOINSECURE",
	"GOTOOLCHAIN", "GO111MODULE", "GOEXPERIMENT", "GOOS", "GOARCH",
	"CGO_ENABLED", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS", "CC", "CXX",
	// Node: the reviewer's own runtime flags (heap size) and module path.
	"NODE_OPTIONS", "NODE_PATH",
	// Python: the environment pyright should look at.
	"VIRTUAL_ENV", "CONDA_PREFIX",
}

// serverEnv builds lang's process environment from environ (os.Environ
// form): the serverEnvKeys entries it holds, then lang.Env, a later entry
// replacing an earlier one with the same key. On Windows keys compare
// case-insensitively, as the OS does.
func serverEnv(lang *Language, environ []string) []string {
	allowed := make(map[string]bool, len(serverEnvKeys))
	for _, k := range serverEnvKeys {
		allowed[envKey(k)] = true
	}
	index := make(map[string]int)
	var out []string
	set := func(kv string) {
		key := envKey(kv)
		if i, ok := index[key]; ok {
			out[i] = kv
			return
		}
		index[key] = len(out)
		out = append(out, kv)
	}
	for _, kv := range environ {
		if allowed[envKey(kv)] {
			set(kv)
		}
	}
	for _, kv := range lang.Env {
		set(kv)
	}
	return out
}

// envKey returns the comparison key of a KEY=VALUE entry (or bare KEY).
func envKey(kv string) string {
	key, _, _ := strings.Cut(kv, "=")
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
