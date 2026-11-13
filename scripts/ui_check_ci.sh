#!/usr/bin/env bash
# The UI check the ui-check CI job runs, and anyone can run locally from the
# repository root: build the server with the UI from internal/server/webui/dist,
# start a disposable experience environment, and walk it with
# ui/tests/browser/ui-check.mjs. On a merge request only the pages the change
# touches are walked (scripts/ui_antd_inventory.mjs --affected); a change to a
# shared file walks them all. With UI_CHECK_COMPARE=1 the target branch's UI is
# built too and served against the same environment, so the report shows the
# states that look different for a person to confirm.
#
#   scripts/ui_check_ci.sh                 # every page, no comparison
#   UI_CHECK_BASE_REF=origin/master scripts/ui_check_ci.sh
#   UI_CHECK_COMPARE=1 UI_CHECK_BASE_REF=origin/master scripts/ui_check_ci.sh
#
# Needs Go, Node and a Chromium (CHROME_PATH, or Chrome found by Playwright).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
out=${UI_CHECK_OUT:-$root/ui-check-report}
work=$(mktemp -d)
port=${UI_CHECK_PORT:-8899}
compare_port=$((port + 1))
pids=()
cleanup() { for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done; rm -f "$root/ui/node_modules/.ui-check-preview.mjs"; git -C "$root" worktree remove --force "$work/base" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT

# CI brings the UI built from this commit (the core-ui artifact). Locally,
# rebuild when the sources are newer than the build, or the check would walk
# an old UI and report faults already fixed.
if [ -z "${CI:-}" ] && { [ ! -f internal/server/webui/dist/index.html ] || [ -n "$(find ui/src ui/index.html -newer internal/server/webui/dist/index.html -print -quit)" ]; }; then
  echo "ui-check: the built UI is older than ui/src; rebuilding"
  (cd ui && npm run build > "$work/ui-build.log" 2>&1) || { tail -30 "$work/ui-build.log" >&2; exit 2; }
fi
for p in "$port" "$compare_port"; do
  if curl -fsS -o /dev/null --max-time 2 "http://127.0.0.1:$p/" 2>/dev/null; then echo "ui-check: port $p is in use; stop what listens there or set UI_CHECK_PORT" >&2; exit 2; fi
done
test -f internal/server/webui/dist/index.html || { echo "ui-check: build the UI first (make ui, or the core-ui artifact)" >&2; exit 2; }

routes_arg=()
if [ -n "${UI_CHECK_BASE_REF:-}" ]; then
  node scripts/ui_antd_inventory.mjs --base "$UI_CHECK_BASE_REF" --affected > "$work/affected.json"
  echo "ui-check: pages to walk: $(cat "$work/affected.json")"
  if [ "$(cat "$work/affected.json")" = "[]" ]; then echo "ui-check: the change touches no page"; exit 0; fi
  routes_arg=(--affected "$work/affected.json")
fi

# The experience environment installs this host's agent from the server's
# embedded set (internal/server/agentbin/dl, filled by make cross-agents). A
# fresh checkout, as on CI, has none: embed just the host's. cross-agents
# replaces the whole embedded set, so it runs only when this one is missing.
host_platform="$(go env GOOS)/$(go env GOARCH)"
if [ ! -f "internal/server/agentbin/dl/gpuworks-agent-$(go env GOOS)-$(go env GOARCH).zst" ]; then
  echo "ui-check: embedding the $host_platform agent"
  make cross-agents PLATFORMS="$host_platform" > "$work/agent-build.log" 2>&1 || { tail -30 "$work/agent-build.log" >&2; exit 2; }
fi
echo "ui-check: building the server"
CGO_ENABLED=0 go build -buildvcs=false -o "$work/gpuworks" ./cmd/gpuworks
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$work/pw"
"$work/gpuworks" experience --data "$work/data" --password-file "$work/pw" --addr "127.0.0.1:$port" > "$work/server.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 60); do curl -fsS -o /dev/null "http://127.0.0.1:$port/" && break; sleep 1; done
curl -fsS -o /dev/null "http://127.0.0.1:$port/" || { echo "ui-check: the experience server did not start" >&2; tail -40 "$work/server.log" >&2; exit 2; }

compare_arg=()
if [ "${UI_CHECK_COMPARE:-0}" = "1" ] && [ -n "${UI_CHECK_BASE_REF:-}" ]; then
  echo "ui-check: building the UI of $UI_CHECK_BASE_REF for comparison"
  git worktree add --detach -q "$work/base" "$UI_CHECK_BASE_REF"
  # The offline benchmark report is built first (ui/package.json "build"); the
  # console imports it from node_modules/.cache, here the shared install's, which
  # every build of this checkout rebuilds before use.
  (cd "$work/base/ui" && ln -s "$root/ui/node_modules" node_modules \
    && { [ ! -f vite.report.config.ts ] || npx vite build --config vite.report.config.ts; } \
    && npx vite build --outDir "$work/base-dist" --emptyOutDir) > "$work/base-build.log" 2>&1 \
    || { echo "ui-check: could not build $UI_CHECK_BASE_REF; comparing skipped" >&2; tail -20 "$work/base-build.log" >&2; }
  if [ -f "$work/base-dist/index.html" ]; then
    # Inside ui/node_modules so the script resolves vite from the UI's own install.
    preview="$root/ui/node_modules/.ui-check-preview.mjs"
    cat > "$preview" <<EOF
import { preview } from "vite";
const server = await preview({ root: "$work/base/ui", configFile: false, build: { outDir: "$work/base-dist" },
  preview: { host: "127.0.0.1", port: $compare_port, strictPort: true, proxy: { "^/api(?:/|$)": { target: "http://127.0.0.1:$port", ws: true } } } });
server.printUrls();
EOF
    # exec: the recorded pid is node itself, so cleanup stops it rather than a subshell.
    (cd "$root/ui" && exec node "$preview") > "$work/preview.log" 2>&1 &
    pids+=($!)
    for _ in $(seq 1 30); do curl -fsS -o /dev/null "http://127.0.0.1:$compare_port/" 2>/dev/null && break; sleep 1; done
    if curl -fsS -o /dev/null "http://127.0.0.1:$compare_port/" 2>/dev/null; then
      compare_arg=(--compare "http://127.0.0.1:$compare_port")
    else
      echo "ui-check: the $UI_CHECK_BASE_REF UI did not start; comparing skipped" >&2; tail -20 "$work/preview.log" >&2
    fi
  fi
fi

cd ui
status=0
node tests/browser/ui-check.mjs --base "http://127.0.0.1:$port" --password-file "$work/pw" \
  --widths "${UI_CHECK_WIDTHS:-1440,1024}" --locales "${UI_CHECK_LOCALES:-zh,en}" \
  --out "$out" ${routes_arg[@]+"${routes_arg[@]}"} ${compare_arg[@]+"${compare_arg[@]}"} || status=$?
git -C "$root" worktree remove --force "$work/base" 2>/dev/null || true
if [ "$status" -eq 0 ] && [ ! -s "$out/results.json" ]; then echo "ui-check: the checker produced no results; failing" >&2; status=2; fi
exit "$status"
