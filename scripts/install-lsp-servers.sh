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
# Outside CI, put both bin-dir and bin-dir/npm/bin on PATH and export
# NPM_CONFIG_PREFIX=bin-dir/npm; the script prints the lines to use.
set -euo pipefail

GOPLS_VERSION=v0.23.0
TSLS_VERSION=6.0.2
TYPESCRIPT_VERSION=6.0.3
PYRIGHT_VERSION=1.1.414
TERRAFORM_LS_VERSION=0.39.0
INTELEPHENSE_VERSION=1.18.5
DART_VERSION=3.13.5

BIN="${1:-$HOME/.local/lsp/bin}"
mkdir -p "$BIN"

GOBIN="$BIN" go install "golang.org/x/tools/gopls@${GOPLS_VERSION}"

# npm's global prefix is often root-owned; keep its packages under BIN too.
# The prefix is exported, not passed as --prefix, so `npm root -g` (crit's
# peek root for global TypeScript libs) points at the same place.
export NPM_CONFIG_PREFIX="$BIN/npm"
npm install -g --no-audit --no-fund \
  "typescript-language-server@${TSLS_VERSION}" \
  "typescript@${TYPESCRIPT_VERSION}" \
  "pyright@${PYRIGHT_VERSION}" \
  "intelephense@${INTELEPHENSE_VERSION}"

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

# Dart: the analysis server ships inside the SDK (`dart language-server`), so
# the whole SDK comes from dart-archive, verified against its .sha256sum. The
# zip unpacks to dart-sdk/; the symlink keeps BIN alone on PATH enough.
case "$os" in darwin) dart_os=macos ;; *) dart_os=$os ;; esac
case "$arch" in amd64) dart_arch=x64 ;; *) dart_arch=$arch ;; esac
dartzip="dartsdk-${dart_os}-${dart_arch}-release.zip"
dartbase="https://storage.googleapis.com/dart-archive/channels/stable/release/${DART_VERSION}/sdk"
curl -fsSL -o "$tmp/$dartzip" "$dartbase/$dartzip"
curl -fsSL -o "$tmp/$dartzip.sha256sum" "$dartbase/$dartzip.sha256sum"
(cd "$tmp" && shasum -a 256 -c "$dartzip.sha256sum")
rm -rf "$BIN/dart-sdk"
unzip -o -q "$tmp/$dartzip" -d "$BIN"
ln -sf "$BIN/dart-sdk/bin/dart" "$BIN/dart"

if [ -n "${GITHUB_PATH:-}" ]; then
  echo "$BIN" >>"$GITHUB_PATH"
  echo "$NPM_CONFIG_PREFIX/bin" >>"$GITHUB_PATH"
  echo "NPM_CONFIG_PREFIX=$NPM_CONFIG_PREFIX" >>"$GITHUB_ENV"
fi

export PATH="$BIN:$NPM_CONFIG_PREFIX/bin:$PATH"
gopls version
typescript-language-server --version
command -v pyright-langserver
terraform-ls version
command -v intelephense
dart --version

if [ -z "${GITHUB_PATH:-}" ]; then
  echo
  echo "export PATH=\"$BIN:$NPM_CONFIG_PREFIX/bin:\$PATH\""
  echo "export NPM_CONFIG_PREFIX=\"$NPM_CONFIG_PREFIX\""
fi
