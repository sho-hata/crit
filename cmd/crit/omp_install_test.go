package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sho-hata/crit/internal/testutil"
)

func TestInstallIntegration_OMP(t *testing.T) {
	for _, global := range []bool{false, true} {
		name := "project"
		if global {
			name = "global"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			project := t.TempDir()
			testutil.SetHome(t, home)
			t.Setenv("PATH", t.TempDir())
			if err := os.Mkdir(filepath.Join(home, ".omp"), 0o755); err != nil {
				t.Fatal(err)
			}
			cwd := project
			skillsDir := filepath.Join(project, ".omp", "skills")
			location := locationProject
			if global {
				cwd = home
				skillsDir = filepath.Join(home, ".omp", "agent", "skills")
				location = locationHome
			}
			t.Chdir(cwd)

			if got := checkMissingIntegrations(project, home); !slices.Equal(got, []string{"omp"}) {
				t.Fatalf("uninstalled omp integration: got %v, want [omp]", got)
			}
			if err := installIntegration("omp", false); err != nil {
				t.Fatal(err)
			}
			if got := checkMissingIntegrations(project, home); len(got) != 0 {
				t.Fatalf("installed integration still missing: %v", got)
			}

			for _, skill := range []string{"crit", "crit-cli", "crit-story"} {
				dest := filepath.Join(skillsDir, skill, "SKILL.md")
				if _, err := os.Stat(dest); err != nil {
					t.Fatalf("skill not installed at omp discovery path %s: %v", dest, err)
				}
			}
			statuses := detectInstalledIntegrations(project, home)
			if len(statuses) != 1 || statuses[0].Agent != "omp" || statuses[0].Status != "current" || statuses[0].Location != location {
				t.Fatalf("installed omp integration not current at %s: %+v", location, statuses)
			}

			// A user-edited skill survives reinstall until --force is requested.
			dest := filepath.Join(skillsDir, "crit", "SKILL.md")
			const custom = "user-edited skill\n"
			if err := os.WriteFile(dest, []byte(custom), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := installIntegration("omp", false); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != custom {
				t.Fatalf("reinstall overwrote user-edited skill: %q", got)
			}
			stale := checkInstalledIntegrations(project, home)
			if len(stale) != 1 || stale[0].agent != "omp" || stale[0].dest != dest || stale[0].location != location {
				t.Fatalf("modified omp skill not detected as stale: %+v", stale)
			}
			if err := installIntegration("omp", true); err != nil {
				t.Fatal(err)
			}
			if stale := checkInstalledIntegrations(project, home); len(stale) != 0 {
				t.Fatalf("forced reinstall left stale skills: %+v", stale)
			}
		})
	}
}
