package lsp

import (
	"slices"
	"testing"
)

func TestServerEnvAllowlist(t *testing.T) {
	t.Parallel()

	environ := []string{
		"PATH=/usr/bin",
		"GITHUB_TOKEN=ghp_secret",
		"HOME=/home/reviewer",
		"AWS_SECRET_ACCESS_KEY=secret",
		"GOTOOLCHAIN=auto",
		"HTTPS_PROXY=http://proxy:3128",
		"ANTHROPIC_API_KEY=secret",
		"GOPATH=/home/reviewer/go",
	}
	lang := &Language{Env: []string{"GOTOOLCHAIN=local"}}
	got := serverEnv(lang, environ)
	// Inherited order, with lang.Env replacing in place.
	want := []string{
		"PATH=/usr/bin",
		"HOME=/home/reviewer",
		"GOTOOLCHAIN=local",
		"HTTPS_PROXY=http://proxy:3128",
		"GOPATH=/home/reviewer/go",
	}
	if !slices.Equal(got, want) {
		t.Errorf("serverEnv = %q, want %q", got, want)
	}
}

func TestServerEnvAppendsLanguageEnv(t *testing.T) {
	t.Parallel()

	lang := &Language{Env: []string{"GOTOOLCHAIN=local"}}
	got := serverEnv(lang, []string{"PATH=/usr/bin"})
	want := []string{"PATH=/usr/bin", "GOTOOLCHAIN=local"}
	if !slices.Equal(got, want) {
		t.Errorf("serverEnv = %q, want %q", got, want)
	}
}

func TestServerEnvNoLanguageEnv(t *testing.T) {
	t.Parallel()

	got := serverEnv(&Language{}, []string{"SECRET=x", "PATH=/bin"})
	if !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Errorf("serverEnv = %q, want only PATH", got)
	}
}
