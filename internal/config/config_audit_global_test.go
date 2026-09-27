package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sho-hata/crit/internal/testutil"
)

func TestAuditGlobalOnlyConfigProjectCannotOverride(t *testing.T) {
	homeDir := t.TempDir()
	testutil.SetHome(t, homeDir)
	projectDir := t.TempDir()

	writeAuditConfig(t, filepath.Join(homeDir, ".crit.config.json"), `{
		"agent_cmd":"global-agent",
		"plan_approve_mode":"acceptEdits",
		"close_on_approve_after_ms":2500,
		"stale_review_days":90,
		"public_url":"https://global-public.example.com",
		"open_cmd":"global-open"
	}`)
	writeAuditConfig(t, filepath.Join(projectDir, ".crit.config.json"), `{
		"agent_cmd":"project-agent",
		"plan_approve_mode":"bypassPermissions",
		"close_on_approve_after_ms":1,
		"stale_review_days":1,
		"public_url":"https://project-public.example.com",
		"open_cmd":"project-open"
	}`)

	cfg := LoadConfig(projectDir)
	assertAuditGlobalValues(t, cfg)
}

func TestAuditGlobalOnlyConfigProjectCannotEnable(t *testing.T) {
	homeDir := t.TempDir()
	testutil.SetHome(t, homeDir)
	projectDir := t.TempDir()

	writeAuditConfig(t, filepath.Join(projectDir, ".crit.config.json"), `{
		"agent_cmd":"project-agent",
		"plan_approve_mode":"bypassPermissions",
		"close_on_approve_after_ms":1,
		"stale_review_days":1,
		"public_url":"https://project-public.example.com",
		"open_cmd":"project-open"
	}`)

	cfg := LoadConfig(projectDir)
	checks := []struct {
		key string
		got string
	}{
		{"agent_cmd", cfg.AgentCmd},
		{"plan_approve_mode", cfg.PlanApproveMode},
		{"public_url", cfg.PublicURL},
		{"open_cmd", cfg.OpenCmd},
	}
	for _, check := range checks {
		if check.got != "" {
			t.Errorf("%s enabled by project config: got %q, want empty", check.key, check.got)
		}
	}
	if cfg.CloseOnApproveAfterMs != nil {
		t.Errorf("close_on_approve_after_ms enabled by project config: got %d, want unset", *cfg.CloseOnApproveAfterMs)
	}
	if cfg.StaleReviewDays != nil {
		t.Errorf("stale_review_days enabled by project config: got %d, want unset", *cfg.StaleReviewDays)
	}
}

func writeAuditConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write config %s: %v", path, err)
	}
}

func assertAuditGlobalValues(t *testing.T, cfg Config) {
	t.Helper()
	checks := []struct {
		key  string
		got  string
		want string
	}{
		{"agent_cmd", cfg.AgentCmd, "global-agent"},
		{"plan_approve_mode", cfg.PlanApproveMode, "acceptEdits"},
		{"public_url", cfg.PublicURL, "https://global-public.example.com"},
		{"open_cmd", cfg.OpenCmd, "global-open"},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s = %q, want global value %q", check.key, check.got, check.want)
		}
	}
	if cfg.CloseOnApproveAfterMs == nil || *cfg.CloseOnApproveAfterMs != 2500 {
		t.Errorf("close_on_approve_after_ms = %v, want global value 2500", cfg.CloseOnApproveAfterMs)
	}
	if cfg.StaleReviewDays == nil || *cfg.StaleReviewDays != 90 {
		t.Errorf("stale_review_days = %v, want global value 90", cfg.StaleReviewDays)
	}
}
