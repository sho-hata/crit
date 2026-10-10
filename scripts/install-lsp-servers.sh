#!/usr/bin/env bash
# install-lsp-servers.sh — install the language servers crit integrates with,
# at pinned versions, for the real-server tests and benchmarks.
#
# Versions are pinned so a server release can't change CI results on its
# own; bump them deliberately. typescript stays on 6.x: 7 is the native
# port and ships no tsserver, which typescript-language-server needs.
#
# Requires go and npm. terraform-ls must be a release build (it embeds the
# provider schemas the tests rely on), so it comes from releases.hashicorp.com
# rather than `go install`.
#
# Usage: bash scripts/install-lsp-servers.sh [bin-dir]   (default ~/.local/lsp/bin)
set -euo pipefail

GOPLS_VERSION=v0.23.0
TSLS_VERSION=6.0.2
TYPESCRIPT_VERSION=6.0.3
PYRIGHT_VERSION=1.1.414
TERRAFORM_LS_VERSION=0.39.0

BIN="${1:-$HOME/.local/lsp/bin}"
mkdir -p "$BIN"

GOBIN="$BIN" go install "golang.org/x/tools/gopls@${GOPLS_VERSION}"

npm install -g --no-audit --no-fund \
  "typescript-language-server@${TSLS_VERSION}" \
  "typescript@${TYPESCRIPT_VERSION}" \
  "pyright@${PYRIGHT_VERSION}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 2 ;;
esac
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
zip="terraform-ls_${TERRAFORM_LS_VERSION}_${os}_${arch}.zip"
base="https://releases.hashicorp.com/terraform-ls/${TERRAFORM_LS_VERSION}"
curl -fsSL -o "$tmp/$zip" "$base/$zip"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/terraform-ls_${TERRAFORM_LS_VERSION}_SHA256SUMS"
(cd "$tmp" && grep " ${zip}\$" SHA256SUMS | shasum -a 256 -c -)
unzip -o -q "$tmp/$zip" terraform-ls -d "$BIN"

if [ -n "${GITHUB_PATH:-}" ]; then
  echo "$BIN" >>"$GITHUB_PATH"
fi

export PATH="$BIN:$PATH"
gopls version
typescript-language-server --version
command -v pyright-langserver
terraform-ls version
