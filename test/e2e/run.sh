#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CRIT_SRC="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"
GIT_PORT="${CRIT_TEST_PORT:-3123}"
GIT2_PORT="${CRIT_TEST_GIT2_PORT:-3131}"
FILE_PORT="${CRIT_TEST_FILE_PORT:-3124}"
SINGLE_PORT="${CRIT_TEST_SINGLE_PORT:-3125}"
NOGIT_PORT="${CRIT_TEST_NOGIT_PORT:-3126}"
MULTI_PORT="${CRIT_TEST_MULTI_PORT:-3127}"
RANGE_PORT="${CRIT_TEST_RANGE_PORT:-3128}"
LIVE_PORT="${CRIT_TEST_LIVE_PORT:-3129}"
PERF_PORT="${CRIT_TEST_PERF_PORT:-3134}"

# Build crit once (skip if CRIT_BIN already points to an existing binary, e.g. CI coverage builds)
if [ -n "${CRIT_BIN:-}" ] && [ -f "$CRIT_BIN" ]; then
  echo "Using pre-built binary: $CRIT_BIN"
else
  BIN_DIR=$(mktemp -d)
  trap 'rm -rf "$BIN_DIR"' EXIT
  export CRIT_BIN="$BIN_DIR/$(e2e_bin_name)"
  (cd "$CRIT_SRC" && go build -o "$CRIT_BIN" ./cmd/crit)
fi

# Ensure the Chromium build matching this project's pinned @playwright/test is
# installed. The browser cache (~/Library/Caches/ms-playwright on macOS) is
# global and version-specific, so a machine that only has another project's
# Playwright version (e.g. crit-web's) won't have the build crit needs and every
# test fails with "Executable doesn't exist". Idempotent — skips the download
# when the build is already cached, so it's a fast no-op in CI and on reruns.
(cd "$SCRIPT_DIR" && npx playwright install chromium)

# E2E_GROUP: all (default) | git (git-mode, sharded by E2E_SHARD) | rest
E2E_GROUP="${E2E_GROUP:-all}"
E2E_SHARD="${E2E_SHARD:-1/1}"
case "$E2E_GROUP" in
  all|git|rest) ;;
  *) echo "Unknown E2E_GROUP: $E2E_GROUP (want all, git or rest)" >&2; exit 2 ;;
esac
[ $# -eq 0 ] || E2E_GROUP=all

# Skip mobile on Windows — Windows headless has reliability issues with
# touchscreen.tap().
RUN_MOBILE=1
if [[ "$OSTYPE" == msys || "$OSTYPE" == cygwin ]]; then
  RUN_MOBILE=0
fi

# Kill any stale processes on our test ports before starting fresh
for port in "$GIT_PORT" "$GIT2_PORT" "$FILE_PORT" "$SINGLE_PORT" "$NOGIT_PORT" "$MULTI_PORT" "$RANGE_PORT" "$LIVE_PORT" "$PERF_PORT"; do
  e2e_kill_port "$port"
done

# Start fixture servers in parallel
cd "$SCRIPT_DIR"
FIXTURE_PIDS=()
FIXTURE_PORTS=()
start_fixture() { # script port
  bash "$1" "$2" &
  FIXTURE_PIDS+=($!)
  FIXTURE_PORTS+=("$2")
}
if [ "$E2E_GROUP" != rest ] || [ "$RUN_MOBILE" -eq 1 ]; then
  start_fixture setup-fixtures.sh "$GIT_PORT"
fi
if [ "$E2E_GROUP" = all ]; then
  start_fixture setup-fixtures.sh "$GIT2_PORT"
fi
if [ "$E2E_GROUP" != git ]; then
  start_fixture setup-fixtures-filemode.sh "$FILE_PORT"
  start_fixture setup-fixtures-singlefile.sh "$SINGLE_PORT"
  start_fixture setup-fixtures-nogit.sh "$NOGIT_PORT"
  start_fixture setup-fixtures-multifile.sh "$MULTI_PORT"
  start_fixture setup-fixtures-range-mode.sh "$RANGE_PORT"
  start_fixture setup-fixtures-livemode.sh "$LIVE_PORT"
  start_fixture setup-fixtures-perf.sh "$PERF_PORT"
fi

cleanup() {
  kill "${FIXTURE_PIDS[@]}" 2>/dev/null || true
  wait "${FIXTURE_PIDS[@]}" 2>/dev/null || true
  # On Git Bash `kill <bash-pid>` doesn't reap the spawned crit.exe child;
  # taskkill /T flushes the whole tree.
  e2e_kill_stray_crit
  rm -rf "${BIN_DIR:-}"
}
trap cleanup EXIT

# Wait for servers to be ready
for port in "${FIXTURE_PORTS[@]}"; do
  while ! curl -sf "http://localhost:$port/api/session" >/dev/null 2>&1; do
    sleep 0.1
  done
done

# Run tests
if [ $# -eq 0 ]; then
  PWLOGS=$(mktemp -d)
  FAILED=0

  # Record each project's real exit code. Playwright exits 0 when a test only
  # flaked and passed on retry, so the log text alone can't tell a recovered
  # flake from a hard failure — both print "failed" in the error detail.
  reap() { # name pid
    local rc=0
    wait "$2" || rc=$?
    echo "$rc" > "$PWLOGS/$1.rc"
    [ "$rc" -eq 0 ] || FAILED=1
  }

  LAUNCHED_NAMES=()
  LAUNCHED_PIDS=()
  REAPED=0
  launch() { # name cmd...
    local name=$1
    shift
    "$@" > "$PWLOGS/$name.log" 2>&1 &
    LAUNCHED_NAMES+=("$name")
    LAUNCHED_PIDS+=($!)
  }
  reap_upto() {
    local n=${1:-${#LAUNCHED_PIDS[@]}}
    while [ "$REAPED" -lt "$n" ]; do
      reap "${LAUNCHED_NAMES[$REAPED]}" "${LAUNCHED_PIDS[$REAPED]}"
      REAPED=$((REAPED + 1))
    done
  }

  if [ "$E2E_GROUP" = git ]; then
    launch git npx playwright test --project=git-mode --shard="$E2E_SHARD"
  elif [ "$E2E_GROUP" = all ]; then
    launch git-1 npx playwright test --project=git-mode --shard=1/2
    launch git-2 env CRIT_TEST_PORT="$GIT2_PORT" npx playwright test --project=git-mode --shard=2/2
  fi
  if [ "$E2E_GROUP" != git ]; then
    launch file   npx playwright test --project=file-mode
    launch single npx playwright test --project=single-file-mode
    launch nogit  npx playwright test --project=no-git-mode
    launch multi  npx playwright test --project=multi-file-mode
    launch range  npx playwright test --project=range-mode
    launch live   npx playwright test --project=live-mode
    launch perf   npx playwright test --project=perf
  fi

  # Mobile shares the git-mode fixture (port 3123) and both projects call
  # DELETE /api/comments in beforeEach, so they must not overlap.
  if [ "$RUN_MOBILE" -eq 1 ] && [ "$E2E_GROUP" != git ]; then
    if [ "$E2E_GROUP" = all ]; then
      reap_upto 2
    fi
    launch mobile npx playwright test --project=mobile
  fi

  reap_upto

  # Print results — show summary for passing projects, full output for failures
  for f in "$PWLOGS"/*.log; do
    name=$(basename "$f" .log)
    rc=$(cat "$PWLOGS/$name.rc" 2>/dev/null || echo 0)
    if [ "$rc" -ne 0 ]; then
      echo "=== $name (FAILED) ==="
      # Dump the full project log on failure so CI shows every error message
      # (a 30-line tail buries per-test errors when many tests fail).
      cat "$f"
    elif grep -q "flaky" "$f"; then
      echo "=== $name (passed, flaky on first attempt) ==="
      tail -5 "$f"
    else
      echo "=== $name ==="
      tail -5 "$f"
    fi
    echo
  done

  rm -rf "$PWLOGS"
  if [ $FAILED -ne 0 ]; then
    echo "Some projects failed. Run 'make e2e-failed' or check individual project logs."
    exit 1
  fi
else
  # Custom args passed: run sequentially as-is
  npx playwright test "$@"
fi
