package lsp

import "os"

// tfNoResultCode is what terraform-ls answers hover, definition and
// references with when nothing is at the position ("position outside of any
// attribute name, value or block", "no reference origin found"): it returns
// plain Go errors, which its JSON-RPC library (jrpc2) reports as SystemError.
// Measured against terraform-ls 0.39.0, about a third of the positions in an
// ordinary .tf file come back this way on hover.
const tfNoResultCode = -32098

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
