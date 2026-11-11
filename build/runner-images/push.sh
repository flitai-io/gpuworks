#!/usr/bin/env bash
# 把一个 registry 里的 gpuworks-runner tag 搬到另一个（多架构整体搬运）。
#
# r1 期间这件事是靠构建机上一个不在仓库里的脚本做的，于是「实际怎么发布的」
# 无法复现也无法审阅。本脚本就是那条流程，收进仓库。
#
# 用法：
#   ./push.sh --dest quay.io/gpuworks/gpuworks-runner --tags-from matrix
#   ./push.sh --dest docker.io/gpuworks/gpuworks-runner --only cuda13.0-vllm0.30.0-r2
#   SRC=192.0.2.156:5000/gpuworks-runner ./push.sh --dest ... --jobs 4
#
# 凭据：先 docker/skopeo login，或传 --creds user:pass（不要写进文件或 CI 日志）。
#
# 注意（2026-10-05 的教训）：
#   - 含厂商 history 的档（hggc/dtk）在 squash 之前**不要**推到任何公网 registry：
#     image config history 里有厂商内网代理的明文凭据。matrix.yaml 里带
#     SQUASH 的档必须用 build.sh --squash 产出的版次。
#   - SWR 拒收 buildkit 默认带 provenance attestation 的 OCI index，故 build.sh
#     一律 --provenance=false --sbom=false。
set -euo pipefail

SRC="${SRC:-registry.zstack.ai/gpuworks/gpuworks-runner}"
HERE="$(cd "$(dirname "$0")" && pwd)"
DEST=""; ONLY=""; JOBS=2; CREDS=""; SRC_INSECURE=0; DEST_INSECURE=0; TAGS_FROM="matrix"; DRY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dest) DEST="$2"; shift 2 ;;
    --only) ONLY="$2"; shift 2 ;;
    --jobs) JOBS="$2"; shift 2 ;;
    --creds) CREDS="$2"; shift 2 ;;
    --tags-from) TAGS_FROM="$2"; shift 2 ;;
    --src-insecure) SRC_INSECURE=1; shift ;;
    --dest-insecure) DEST_INSECURE=1; shift ;;
    --dry-run) DRY=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$DEST" ]] || { echo "--dest required" >&2; exit 2; }
command -v skopeo >/dev/null || command -v /usr/local/bin/skopeo >/dev/null \
  || { echo "skopeo required" >&2; exit 1; }
SKOPEO="$(command -v skopeo || echo /usr/local/bin/skopeo)"

if [[ -n "$ONLY" ]]; then
  TAGS=("$ONLY")
elif [[ "$TAGS_FROM" == "matrix" ]]; then
  readarray -t TAGS < <(python3 - "$HERE/matrix.yaml" <<'PY'
import sys, yaml
m = yaml.safe_load(open(sys.argv[1]))
for it in m["items"]:
    if it.get("builder") == "prebuilt":
        print("SKIP %s: %s is not built into SRC" % (it["id"], it["builder"]), file=sys.stderr)
        continue
    print("%s-%s" % (it["id"], it.get("revision", "r1")))
PY
)
else
  readarray -t TAGS < <(curl -fsSL "https://${SRC%%/*}/v2/${SRC#*/}/tags/list" \
      | python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin).get("tags",[])))')
fi

echo "src=$SRC dest=$DEST tags=${#TAGS[@]} jobs=$JOBS"
[[ ${#TAGS[@]} -gt 0 ]] || exit 0
args=(copy --all --preserve-digests)
[[ $SRC_INSECURE -eq 1 ]] && args+=(--src-tls-verify=false)
[[ $DEST_INSECURE -eq 1 ]] && args+=(--dest-tls-verify=false)
[[ -n "$CREDS" ]] && args+=(--dest-creds "$CREDS")

export SKOPEO SRC DEST DRY
export SKOPEO_ARGS="${args[*]}"

rc=0
printf '%s\n' "${TAGS[@]}" | xargs -P "$JOBS" -I{} bash -c '
  args=($SKOPEO_ARGS)
  if [ "$DRY" = 1 ]; then echo "(dry-run) $SRC:{} -> $DEST:{}"; exit 0; fi
  if "$SKOPEO" "${args[@]}" "docker://$SRC:{}" "docker://$DEST:{}" >/dev/null 2>&1; then
    echo "$(date +%H:%M:%S) OK   {}"
  else
    echo "$(date +%H:%M:%S) FAIL {}"; exit 1
  fi' || rc=1
exit $rc
