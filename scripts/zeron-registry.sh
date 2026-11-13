#!/usr/bin/env bash
# Reproducible local Zeron Registry for ui/.
#
#   scripts/zeron-registry.sh add <names...> [zeron-ui add options]
#   scripts/zeron-registry.sh check
#   scripts/zeron-registry.sh serve
#   scripts/zeron-registry.sh prepare        # fetch + build only
#
# The pinned revision is read from ui/src/theme/zeron/source.json (commit,
# repository). The script fetches exactly that commit into a cache, verifies
# it, installs the CLI's declared dependencies, and builds the static Registry
# from the revision's committed build output (public/r): registryDependencies
# URLs are rebased from https://zeron-ui.vercel.app/r to the loopback server,
# component content is unchanged, and r/manifest.json records both hashes.
#
# Environment:
#   ZERON_CACHE_DIR     cache root for this commit
#                       (default ${XDG_CACHE_HOME:-~/.cache}/gpuworks/zeron/<commit>)
#   ZERON_GIT_URL       fetch from this URL or local clone instead of repository
#   ZERON_REGISTRY_PORT loopback port (default 5211; install-state records the
#                       base URL, so keep the default unless you know why)
set -euo pipefail

die() { printf 'zeron-registry: %s\n' "$*" >&2; exit 1; }
log() { printf 'zeron-registry: %s\n' "$*" >&2; }

usage() {
  sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UI="$ROOT/ui"
PIN_FILE="$UI/src/theme/zeron/source.json"
STATE_FILE="$UI/.zeron/install-state.json"
UPSTREAM_BASE="https://zeron-ui.vercel.app/r"
PORT="${ZERON_REGISTRY_PORT:-5211}"
BASE="http://127.0.0.1:$PORT/r"

# --- prerequisites ---------------------------------------------------------

need_node() {
  command -v node >/dev/null 2>&1 \
    || die "node not found. Install Node.js >= 20.18.1 (the Zeron CLI's engine floor) and retry."
  node -e '
    const [a,b,c] = process.versions.node.split(".").map(Number);
    process.exit(a > 20 || (a === 20 && (b > 18 || (b === 18 && c >= 1))) ? 0 : 1);' \
    || die "Node.js $(node --version) is too old; the Zeron CLI needs >= 20.18.1."
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 not found. $2"
}

[ -f "$PIN_FILE" ] || die "missing $PIN_FILE (pinned Zeron revision)."

pin_field() {
  node -e 'const v = require(process.argv[1])[process.argv[2]];
    if (typeof v !== "string" || !v) process.exit(1); process.stdout.write(v);' "$PIN_FILE" "$1" \
    || die "$PIN_FILE has no \"$1\" field."
}

# --- fetch -----------------------------------------------------------------

fetch_source() {
  need_cmd git "Install git and retry."
  local src="$CACHE/src" url="${ZERON_GIT_URL:-$REPO}" head
  if [ -d "$src/.git" ] && head="$(git -C "$src" rev-parse HEAD 2>/dev/null)" && [ "$head" = "$COMMIT" ]; then
    return 0
  fi
  log "fetching $COMMIT from $url"
  rm -rf "$src.tmp"
  mkdir -p "$src.tmp"
  local gitopts=() proxy
  # Honour the user's own GitHub proxy setting; never hardcode one.
  if proxy="$(git config --get http.https://github.com.proxy 2>/dev/null)" && [ -n "$proxy" ]; then
    gitopts=(-c "http.proxy=$proxy")
  fi
  git -C "$src.tmp" init -q
  if ! git ${gitopts[@]+"${gitopts[@]}"} -C "$src.tmp" fetch -q --depth 1 "$url" "$COMMIT"; then
    rm -rf "$src.tmp"
    die "could not fetch $COMMIT from $url.
  - Check network access to github.com. If you need a proxy, configure it for git:
      git config --global http.https://github.com.proxy http://<proxy-host>:<port>
  - Or fetch from an existing clone: ZERON_GIT_URL=/path/to/zeron-ui $0 ..."
  fi
  git -C "$src.tmp" -c advice.detachedHead=false checkout -q --detach FETCH_HEAD
  head="$(git -C "$src.tmp" rev-parse HEAD)"
  [ "$head" = "$COMMIT" ] || { rm -rf "$src.tmp"; die "fetched $head, expected $COMMIT; refusing to continue."; }
  rm -rf "$src"
  mv "$src.tmp" "$src"
}

# --- CLI dependencies ------------------------------------------------------

install_cli_deps() {
  local cli="$CACHE/src/packages/cli" marker="$CACHE/src/packages/cli/node_modules/.gpuworks-deps-ok"
  [ -f "$marker" ] && return 0
  need_cmd npm "Install npm (ships with Node.js) and retry."
  log "installing Zeron CLI dependencies (declared exact versions in packages/cli/package.json)"
  (cd "$cli" && npm install --omit=dev --ignore-scripts --no-audit --no-fund --no-package-lock --loglevel=error >&2) \
    || die "npm install failed in $cli. Check network access to the npm registry (npm config get registry) and retry."
  : > "$marker"
}

# --- build -----------------------------------------------------------------

build_registry() {
  local out="$CACHE/registry-$PORT"
  [ -f "$out/r/manifest.json" ] && return 0
  log "building Registry for $BASE"
  rm -rf "$out.tmp"
  mkdir -p "$out.tmp/r"
  node - "$CACHE/src" "$out.tmp/r" "$UPSTREAM_BASE" "$BASE" "$COMMIT" "$REGISTRY_SHA" <<'NODE' || { rm -rf "$out.tmp"; die "Registry build failed."; }
const fs = require("fs"), path = require("path"), crypto = require("crypto");
const [src, out, upstream, base, commit, expectedSurfaces] = process.argv.slice(2);
const sha = (b) => crypto.createHash("sha256").update(b).digest("hex");
const dir = path.join(src, "public/r");
const names = fs.readdirSync(dir).filter((n) => n.endsWith(".json")).sort();
if (!names.length) throw new Error(`no Registry items in ${dir}`);
const surfaces = fs.readFileSync(path.join(dir, "surfaces.json"));
if (expectedSurfaces && sha(surfaces) !== expectedSurfaces)
  throw new Error("public/r/surfaces.json does not match registrySha256 in source.json");
const cliVersion = JSON.parse(fs.readFileSync(path.join(src, "packages/cli/package.json"), "utf8")).version;
const files = names.map((name) => {
  const raw = fs.readFileSync(path.join(dir, name));
  const rebased = Buffer.from(raw.toString("utf8").split(upstream).join(base), "utf8");
  fs.writeFileSync(path.join(out, name), rebased);
  return { path: name, sourceSha256: sha(raw), sha256: sha(rebased) };
});
fs.writeFileSync(path.join(out, "manifest.json"), JSON.stringify({
  schemaVersion: 1,
  kind: "local-pinned-registry",
  sourceRevision: commit,
  cliVersion,
  baseUrl: base,
  transformation: "Only registryDependencies URLs rebased to loopback; component source content unchanged. Not an upstream public release.",
  files,
}, null, 2) + "\n");
NODE
  rm -rf "$out"
  mv "$out.tmp" "$out"
}

prepare() {
  need_node
  COMMIT="$(pin_field commit)"
  REPO="$(pin_field repository)"
  REGISTRY_SHA="$(node -e 'process.stdout.write(require(process.argv[1]).registrySha256 || "")' "$PIN_FILE")"
  printf '%s' "$COMMIT" | grep -Eq '^[0-9a-f]{40}$' || die "commit in $PIN_FILE is not a full 40-character SHA."
  CACHE="${ZERON_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/gpuworks/zeron/$COMMIT}"
  mkdir -p "$CACHE"
  fetch_source
  install_cli_deps
  build_registry
  REGISTRY_DIR="$CACHE/registry-$PORT"
  CLI="$CACHE/src/packages/cli/src/index.js"
}

# --- server ----------------------------------------------------------------

SERVER_JS='
const http = require("http"), fs = require("fs"), path = require("path");
const [root, port] = process.argv.slice(1);
const server = http.createServer((req, res) => {
  const url = decodeURIComponent((req.url || "/").split("?")[0]);
  const file = path.join(root, path.normalize(url));
  if (!file.startsWith(root + path.sep) || !url.startsWith("/r/")) { res.writeHead(404); return res.end(); }
  fs.readFile(file, (err, body) => {
    if (err) { res.writeHead(404); return res.end(); }
    res.writeHead(200, { "content-type": "application/json; charset=utf-8" });
    res.end(req.method === "HEAD" ? undefined : body);
  });
});
server.on("error", (e) => { console.error(e.message); process.exit(1); });
server.listen(Number(port), "127.0.0.1", () => console.log("ready"));
for (const s of ["SIGINT", "SIGTERM"]) process.on(s, () => server.close(() => process.exit(0)));
'

# Prints "same" if the port already serves this revision's Registry, "busy"
# if something else holds it, "free" otherwise.
port_state() {
  node -e '
    const [base, commit] = process.argv.slice(1);
    fetch(base + "/manifest.json", { signal: AbortSignal.timeout(2000) })
      .then((r) => r.ok ? r.json() : null)
      .then((m) => console.log(m && m.sourceRevision === commit && m.baseUrl === base ? "same" : "busy"))
      .catch((e) => console.log(/ECONNREFUSED/.test(String(e.cause && e.cause.code || e.cause || e)) ? "free" : "busy"));' "$BASE" "$COMMIT"
}

SERVER_PID=""
stop_server() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
    SERVER_PID=""
  fi
}

# Start a temporary server unless the same Registry is already being served.
ensure_server() {
  case "$(port_state)" in
    same) log "reusing Registry already served at $BASE"; return 0 ;;
    busy) die "127.0.0.1:$PORT is in use by something other than Zeron $COMMIT. Stop it, or let it serve this revision (\"$0 serve\")." ;;
  esac
  local ready="$CACHE/server.$$.log" i
  : > "$ready"
  node -e "$SERVER_JS" "$REGISTRY_DIR" "$PORT" > "$ready" 2>&1 &
  SERVER_PID=$!
  trap 'stop_server; rm -f "'"$ready"'"' EXIT
  trap 'exit 130' INT TERM
  for i in $(seq 1 50); do
    grep -q ready "$ready" 2>/dev/null && return 0
    kill -0 "$SERVER_PID" 2>/dev/null || die "Registry server failed: $(cat "$ready")"
    sleep 0.1
  done
  die "Registry server did not start on 127.0.0.1:$PORT."
}

# --- managed file hashes ---------------------------------------------------

verify_install_state() {
  [ -f "$STATE_FILE" ] || die "missing $STATE_FILE."
  node - "$UI" "$STATE_FILE" "$CLI" <<'NODE'
const fs = require("fs"), path = require("path"), crypto = require("crypto");
const [ui, stateFile, cli] = process.argv.slice(2);
const state = JSON.parse(fs.readFileSync(stateFile, "utf8"));
const cliVersion = JSON.parse(fs.readFileSync(path.join(path.dirname(cli), "../package.json"), "utf8")).version;
const latest = new Map();
for (const entry of state.installations || []) {
  if (entry.cliVersion && entry.cliVersion !== cliVersion)
    console.log(`! install record ${entry.items.join(",")} came from CLI ${entry.cliVersion}, pinned CLI is ${cliVersion}`);
  for (const [file, h] of Object.entries(entry.fileHashes || {})) latest.set(file, h.installedHash); // later records win
}
const bad = [];
for (const [file, expected] of latest) {
  const p = path.join(ui, file);
  if (!fs.existsSync(p)) { bad.push(`missing  ${file}`); continue; }
  const actual = crypto.createHash("sha256").update(fs.readFileSync(p)).digest("hex");
  if (actual !== expected) bad.push(`modified ${file}`);
}
for (const line of bad) console.log(`✗ ${line}`);
console.log(`${bad.length ? "✗" : "✓"} managed files match install-state (${latest.size - bad.length}/${latest.size})`);
if (bad.length) {
  console.log("  Managed Zeron sources must not be edited. Restore them (git checkout -- <file>) or reinstall with \"add <name> --overwrite\".");
  process.exit(1);
}
NODE
}

# --- commands --------------------------------------------------------------

cmd="${1:-}"
[ $# -gt 0 ] && shift
case "$cmd" in
  prepare)
    prepare
    log "Registry: $REGISTRY_DIR/r  CLI: $CLI"
    ;;
  serve)
    prepare
    case "$(port_state)" in
      same) die "Registry for $COMMIT is already served at $BASE." ;;
      busy) die "127.0.0.1:$PORT is already in use. Stop that process first." ;;
    esac
    log "serving $REGISTRY_DIR/r at $BASE (Ctrl-C to stop)"
    exec node -e "$SERVER_JS" "$REGISTRY_DIR" "$PORT"
    ;;
  add)
    [ $# -gt 0 ] || die "usage: $0 add <names...> [--dry-run|--overwrite|...]"
    prepare
    ensure_server
    node "$CLI" add "$@" --cwd "$UI" --yes --registry "$BASE"
    ;;
  check)
    prepare
    ensure_server
    status=0
    node "$CLI" doctor --check --cwd "$UI" --registry "$BASE" || status=1
    verify_install_state || status=1
    exit "$status"
    ;;
  -h|--help|help) usage 0 ;;
  *) usage 1 ;;
esac
