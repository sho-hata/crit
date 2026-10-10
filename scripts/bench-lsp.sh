#!/usr/bin/env bash
# bench-lsp.sh — run BenchmarkRealLSP for a base commit and the working tree,
# interleaved on the same machine, and write benchstat inputs.
#
# Real language servers are far noisier than in-process benches, and shared
# CI runners drift over a job's lifetime, so the two sides alternate round by
# round (order flipped every round) instead of running back to back. The base
# is built with the working tree's real-server test files, so both sides run
# the same benchmark code and only the implementation differs. When those
# files don't compile against the base (an API change), the base's own copy
# is used instead.
#
# Requires the language servers on PATH (scripts/install-lsp-servers.sh).
#
# Usage: bash scripts/bench-lsp.sh <base-ref> [rounds] [out-dir]
#   then: benchstat <out-dir>/bench-old.txt <out-dir>/bench-new.txt
set -euo pipefail

BASE="${1:?usage: bench-lsp.sh <base-ref> [rounds] [out-dir]}"
ROUNDS="${2:-6}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${3:-$ROOT}"
WORK="$(mktemp -d)"
BASE_TREE="$WORK/base"

cleanup() {
  git -C "$ROOT" worktree remove --force "$BASE_TREE" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

cd "$ROOT"
go test -c -o "$WORK/new.test" ./internal/lsp

git worktree add --detach "$BASE_TREE" "$BASE" >/dev/null
cp internal/lsp/real_*_manual_test.go "$BASE_TREE/internal/lsp/"
if ! (cd "$BASE_TREE" && go test -c -o "$WORK/old.test" ./internal/lsp); then
  echo "::warning::working-tree LSP benchmarks don't build against $BASE; using the base's own"
  git -C "$BASE_TREE" checkout -- internal/lsp
  git -C "$BASE_TREE" clean -fdq -- internal/lsp
  (cd "$BASE_TREE" && go test -c -o "$WORK/old.test" ./internal/lsp)
fi

export CRIT_LSP_REAL=1
: >"$OUT/bench-new.txt"
: >"$OUT/bench-old.txt"

# Cold starts take a few hundred ms each, so they get a fixed iteration
# count; warm requests are sub-millisecond and use a short time budget.
run() {
  local bin="$1" out="$2"
  (cd "$WORK" && "$bin" -test.run='^$' -test.bench='BenchmarkRealLSP/.*/cold' -test.benchtime=3x) >>"$out"
  (cd "$WORK" && "$bin" -test.run='^$' -test.bench='BenchmarkRealLSP/.*/(hover|definition|references)' -test.benchtime=500ms) >>"$out"
}

for i in $(seq 1 "$ROUNDS"); do
  echo "round $i/$ROUNDS"
  if [ $((i % 2)) -eq 1 ]; then
    run "$WORK/new.test" "$OUT/bench-new.txt"
    run "$WORK/old.test" "$OUT/bench-old.txt"
  else
    run "$WORK/old.test" "$OUT/bench-old.txt"
    run "$WORK/new.test" "$OUT/bench-new.txt"
  fi
done

# A benchmark that skipped (server missing) leaves no rows; refuse to pass blind.
if ! grep -q '^BenchmarkRealLSP' "$OUT/bench-new.txt"; then
  echo "error: no BenchmarkRealLSP results — are the language servers on PATH?" >&2
  exit 2
fi
