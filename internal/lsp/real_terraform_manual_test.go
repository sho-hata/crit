package lsp

// Opt-in integration tests against a real terraform-ls (same CRIT_LSP_REAL
// gate as the other real-server tests). Skipped unless enabled:
//
//	CRIT_LSP_REAL=1 go test ./internal/lsp -run TestRealTerraform -v
//
// Requires a release build of terraform-ls on PATH (Homebrew, the HashiCorp
// releases page): those embed the provider schemas the resource-body
// assertions rely on. A `go install` build has none. terraform itself is not
// needed — crit makes sure it is never run.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const tfVersionsSource = `terraform {
  required_providers {
    random = {
      source = "hashicorp/random"
    }
  }
}
`

const tfMainSource = `variable "region" {
  type        = string
  description = "AWS region to deploy into"
}

resource "random_pet" "web" {
  prefix = var.region
}

module "net" {
  source = "./modules/net"
}

output "vpc" {
  value = module.net.vpc_id
}
`

const tfNetSource = `output "vpc_id" {
  description = "The VPC id"
  value       = "vpc-123"
}
`

// Positions in tfMainSource (0-based line, UTF-16 character).
const (
	tfRegionUseLine, tfRegionUseChar = 6, 17  // var.region inside random_pet
	tfRegionDeclLine, tfRegionDecl   = 0, 12  // "region" in the variable label
	tfVpcIDLine, tfVpcIDChar         = 14, 22 // vpc_id in module.net.vpc_id
	tfBlankLine, tfBlankChar         = 4, 0   // the empty line between blocks
)

func requireRealTerraformLS(t *testing.T) {
	t.Helper()
	if os.Getenv("CRIT_LSP_REAL") == "" {
		t.Skip("set CRIT_LSP_REAL=1 to run against a real terraform-ls")
	}
	if !LanguageForPath("x.tf").Available() {
		t.Fatal("terraform-ls not on PATH")
	}
}

// writeTFProject lays out a root module with a local child module and
// returns the path of main.tf.
func writeTFProject(t *testing.T, dir string) string {
	t.Helper()
	files := map[string]string{
		"versions.tf":         tfVersionsSource,
		"main.tf":             tfMainSource,
		"modules/net/main.tf": tfNetSource,
	}
	for name, src := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "main.tf")
}

func TestRealTerraformLS(t *testing.T) {
	t.Parallel()
	requireRealTerraformLS(t)

	dir := t.TempDir()
	main := writeTFProject(t, dir)

	m := NewManager(dir, "", context.Background())
	defer m.Shutdown()

	// A reference inside a resource body only resolves once the provider's
	// schema is known, and terraform is never run here: this proves the
	// embedded schemas are in use. It is also the first request, so it shows
	// the server holds requests until the directory is indexed.
	start := time.Now()
	got, err := m.Hover(main, tfRegionUseLine, tfRegionUseChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	t.Logf("first hover took %s: %q", time.Since(start).Round(time.Millisecond), got)
	if !strings.Contains(got, "var.region") || !strings.Contains(got, "string") {
		t.Errorf("hover = %q, want var.region's type", got)
	}

	locs, err := m.Definition(main, tfRegionUseLine, tfRegionUseChar)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 || locs[0].Path != main || locs[0].Line != tfRegionDeclLine {
		t.Errorf("definition = %+v, want the variable block in main.tf", locs)
	}

	// terraform-ls answers references from the declaration, not from a use.
	refs, err := m.References(main, tfRegionDeclLine, tfRegionDecl)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) != 1 || refs[0].Line != tfRegionUseLine {
		t.Errorf("references = %+v, want the use inside random_pet", refs)
	}

	// Across a local module: the output's description comes from the child.
	got, err = m.Hover(main, tfVpcIDLine, tfVpcIDChar)
	if err != nil {
		t.Fatalf("Hover(module output): %v", err)
	}
	if !strings.Contains(got, "The VPC id") {
		t.Errorf("hover = %q, want the child module's output description", got)
	}
	locs, err = m.Definition(main, tfVpcIDLine, tfVpcIDChar)
	if err != nil {
		t.Fatalf("Definition(module output): %v", err)
	}
	want := filepath.Join(dir, "modules", "net", "main.tf")
	found := false
	for _, l := range locs {
		found = found || l.Path == want
	}
	if !found {
		t.Errorf("definition = %+v, want %s among them", locs, want)
	}

	// Nothing at the position: terraform-ls errors, crit must not.
	got, err = m.Hover(main, tfBlankLine, tfBlankChar)
	if err != nil || got != "" {
		t.Errorf("Hover(blank line) = %q, %v; want empty and no error", got, err)
	}
	locs, err = m.Definition(main, tfBlankLine, tfBlankChar)
	if err != nil || len(locs) != 0 {
		t.Errorf("Definition(blank line) = %+v, %v; want none and no error", locs, err)
	}
}

// fakeTerraformOnPath puts a `terraform` first on PATH that only records that
// it ran, and returns the marker file it creates.
func fakeTerraformOnPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake terraform is a shell script")
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "terraform-ran")
	script := "#!/bin/sh\necho \"$@\" >> '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

// writeInitedTFProject is writeTFProject plus the .terraform/ that
// `terraform init` leaves behind, which is what makes terraform-ls ask
// terraform for provider schemas.
func writeInitedTFProject(t *testing.T, dir string) string {
	t.Helper()
	main := writeTFProject(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".terraform", "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	return main
}

// In an init'ed directory terraform-ls runs `terraform providers schema`,
// which starts the provider binaries under .terraform/ — the tree under
// review. crit must keep it from running terraform at all. Not parallel:
// it changes PATH.
func TestRealTerraformLSNeverRunsTerraform(t *testing.T) {
	requireRealTerraformLS(t)
	marker := fakeTerraformOnPath(t)

	dir := t.TempDir()
	main := writeInitedTFProject(t, dir)

	m := NewManager(dir, "", context.Background())
	defer m.Shutdown()

	got, err := m.Hover(main, tfRegionUseLine, tfRegionUseChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if !strings.Contains(got, "var.region") {
		t.Errorf("hover = %q, want var.region from the embedded schemas", got)
	}
	// terraform-ls asks terraform from a background job that a hover does not
	// wait for, so give it the time the control below needs, with margin.
	if ran, ok := waitForMarker(marker, tfRunWindow); ok {
		t.Errorf("terraform was run: %q", ran)
	}
}

// tfRunWindow is how long to wait for terraform-ls to run terraform. The
// control sees it within half a second.
const tfRunWindow = 5 * time.Second

// waitForMarker polls for the fake terraform's marker file for up to d.
func waitForMarker(marker string, d time.Duration) ([]byte, bool) {
	deadline := time.Now().Add(d)
	for {
		if ran, err := os.ReadFile(marker); err == nil {
			return ran, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Control: the same directory with terraform-ls's own defaults does run the
// PATH terraform, so the test above is not passing vacuously.
func TestRealTerraformLSRunsTerraformByDefault(t *testing.T) {
	requireRealTerraformLS(t)
	marker := fakeTerraformOnPath(t)

	dir := t.TempDir()
	main := writeInitedTFProject(t, dir)

	c, err := startServer(context.Background(), dir, LanguageForPath(main), nil, nil)
	if err != nil {
		t.Fatalf("startServer: %v", err)
	}
	defer c.Close()
	if err := c.DidOpen(main, "terraform", tfMainSource, 1); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	if _, err := c.Hover(main, tfRegionUseLine, tfRegionUseChar); err != nil {
		t.Logf("Hover: %v", err)
	}
	start := time.Now()
	ran, ok := waitForMarker(marker, tfRunWindow)
	if !ok {
		t.Fatal("terraform was never run; the init'ed fixture no longer triggers it")
	}
	t.Logf("terraform ran %s after the hover with: %q", time.Since(start).Round(time.Millisecond), ran)
}
