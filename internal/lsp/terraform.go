package lsp

import (
	"os"
	"strings"
)

// tfSystemErrorCode is the code terraform-ls's JSON-RPC library (jrpc2)
// gives every plain Go error. It is generic: real failures share it.
const tfSystemErrorCode = -32098

// tfNoResultMessages are the errors terraform-ls (via hcl-lang) answers hover,
// definition and references with when nothing is at the position. Measured
// against terraform-ls 0.39.0, about a third of the positions in an ordinary
// .tf file come back this way on hover.
var tfNoResultMessages = []string{
	"position outside of",       // whitespace, block headers, between blocks
	"unknown attribute",         // attribute missing from the schema
	"unknown block type",        // block missing from the schema
	"unexpected label",          // label beyond the schema's
	"no reference origin found", // definition on a non-reference
	"no reference target found", // reference whose target is not indexed
}

// tfNoResult reports whether a terraform-ls error means "nothing here". Only
// the known messages count: anything else with the same code (a file not
// found, no schema available) is a real failure and must surface.
func tfNoResult(e *ResponseError) bool {
	if e.Code != tfSystemErrorCode {
		return false
	}
	for _, msg := range tfNoResultMessages {
		if strings.Contains(e.Message, msg) {
			return true
		}
	}
	return false
}

// tfInitOptions stops terraform-ls from running terraform.
//
// When a directory has a .terraform/ (it was `terraform init`ed), terraform-ls
// runs `terraform providers schema -json` there, and terraform starts the
// provider plugin binaries under .terraform/providers to answer. Those come
// from the tree under review, so a branch committing .terraform/ with a
// matching .terraform.lock.hcl would run code just by being opened in a diff
// — the same reason pyConfigSettings never sends python.pythonPath.
//
// os.DevNull exists, so the initialize-time stat passes, and cannot be
// executed, so every terraform run fails and terraform-ls falls back to the
// provider schemas embedded in its release build (official and partner
// providers, at their latest version). Providers missing from that set get no
// resource schema: references inside their blocks don't resolve. Pointing at
// a path that does not exist instead makes initialize fail.
func tfInitOptions(_, _ string) map[string]any {
	return map[string]any{"terraform": map[string]any{"path": os.DevNull}}
}
