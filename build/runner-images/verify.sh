#!/usr/bin/env bash
# gpuworks-runner 成品镜像验证（CPU-only，无需 GPU）
#
# 判定标准不在本脚本里，而在镜像自带的 /opt/gpuworks/image.json（构建时写入的
# requires 清单）与 /opt/gpuworks/shared/assert_requires.sh（唯一实现）。这样
# 「构建时断言过的东西」与「成品里验的东西」不可能各说各话。
#
# 用法：
#   ./verify.sh registry.zstack.ai/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2 [更多...]
#   ./verify.sh --all            # 验 REGISTRY 下 matrix.yaml 列出的全部不可变 tag
#
# 退出码非零 = 有镜像不满足它自己声明的 requires，或缺少自描述清单。
set -uo pipefail

REGISTRY="${REGISTRY:-registry.zstack.ai/gpuworks/gpuworks-runner}"
HERE="$(cd "$(dirname "$0")" && pwd)"
FAIL=0

verify_one() {
  local ref="$1"
  echo "=== $ref"
  # 本地已有就直接验（--no-push 构建出来的镜像只在 daemon 里，没法 pull）
  if ! docker image inspect "$ref" >/dev/null 2>&1; then
    if ! docker pull -q "$ref" >/dev/null 2>&1; then
      echo "  FAIL: not present locally and cannot pull"; FAIL=1; return
    fi
  fi

  # 1) 自描述清单必须存在（r1 的镜像没有，验到就说明拿到的是旧版次）
  local manifest
  manifest="$(docker run --rm --entrypoint cat "$ref" /opt/gpuworks/image.json 2>/dev/null)"
  if [ -z "$manifest" ]; then
    echo "  FAIL: /opt/gpuworks/image.json missing —— 这是 r1 或非本仓构建的镜像"
    FAIL=1; return
  fi
  python3 - "$manifest" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
print("  id=%s rev=%s engine=%s framework=%s requires=%s"
      % (d.get("id"), d.get("revision"), d.get("engine"),
         d.get("framework"), ",".join(d.get("requires") or [])))
PY

  # 2) 用镜像自带的断言脚本按自带的 requires 复验
  if ! docker run --rm --entrypoint bash "$ref" -c '
        set -eo pipefail
        eval "$(python3 - <<PY
import json
d = json.load(open("/opt/gpuworks/image.json"))
print("REQUIRES=%r" % " ".join(d.get("requires") or []))
print("ENGINE=%r" % (d.get("engine") or ""))
print("ENGINE_VERSION=%r" % (d.get("engine_version") or ""))
print("TORCH_VERSION=%r" % (d.get("torch") or ""))
print("TRANSFORMERS_VERSION=%r" % (d.get("transformers") or ""))
PY
)"
        export REQUIRES ENGINE ENGINE_VERSION TORCH_VERSION TRANSFORMERS_VERSION
        bash /opt/gpuworks/shared/assert_requires.sh
      ' 2>&1 | sed 's/^/  /'; then
    echo "  FAIL: requires assertion failed"; FAIL=1
  fi

  # 3) 补丁清单与对 base 守护包的显式替换（有则打印，便于和 matrix 的预期对账）
  docker run --rm --entrypoint sh "$ref" -c \
      'for f in /opt/gpuworks/patches-*.json /opt/gpuworks/base-replaced.json; do [ -f "$f" ] && echo "$f: $(cat "$f" | tr -d "\n ")"; done' \
      2>/dev/null | sed 's/^/  /'

  # 4) 不该出现在成品里的东西
  #
  # .rej/.orig 只在**补丁实际操作的目录**（引擎的 site-packages）里才算问题。
  # 早先这里扫全盘，于是厂商 base 自带的 miniforge3/jupyterlab 的 .orig 把每个厂商
  # 档都报成 WARN——这种噪音只会训练人忽略告警。
  docker run --rm --entrypoint sh "$ref" -c '
      if [ -n "${PIP_INDEX_URL:-}" ]; then
        echo "  WARN: PIP_INDEX_URL leaked into runtime env: ${PIP_INDEX_URL}"
      fi
      site=$(python3 -c "import importlib.util as u, json, os; e=json.load(open(\"/opt/gpuworks/image.json\"))[\"engine\"]; s=u.find_spec(e); print(os.path.dirname(os.path.dirname(s.origin)) if s and s.origin else \"\")" 2>/dev/null)
      # 只看补丁真正动过的包目录。厂商 base 在 site-packages 里自带的 .orig
      # （jupyterlab、miniforge 等）与我们无关，扫进来只会制造噪音。
      dirs=""
      for pkg in vllm vllm_omni sglang; do
        [ -d "$site/$pkg" ] && dirs="$dirs $site/$pkg"
      done
      # 昇腾 omni 档的 vllm_omni 是 editable 安装（/vllm-workspace/vllm-omni），不在引擎的 site 目录里
      osite=$(python3 -c "import importlib.util as u, os; s=u.find_spec(\"vllm_omni\"); print(os.path.dirname(os.path.dirname(s.origin)) if s and s.origin else \"\")" 2>/dev/null)
      [ -n "$osite" ] && [ "$osite" != "$site" ] && [ -d "$osite/vllm_omni" ] && dirs="$dirs $osite/vllm_omni"
      if [ -n "$dirs" ]; then
        found=$(find $dirs -name "*.rej" -o -name "*.orig" 2>/dev/null | head -5)
        if [ -n "$found" ]; then
          echo "  WARN: leftover patch files in patched package dirs:"
          echo "$found" | sed "s|^|      |"
        else
          echo "  patch leftovers: none in$dirs"
        fi
      fi
      exit 0' 2>/dev/null

  # 5) image config history 里不得含凭据（厂商 base 的已知问题，靠 squash 解决）
  # 同上：不要用 .ContainerConfig（新版 Docker 已移除，会让检查静默失效）
  { docker image inspect "$ref" --format '{{json .Config}}' 2>/dev/null
    docker history --no-trunc --format '{{.CreatedBy}}' "$ref" 2>/dev/null; } \
    | python3 -c "
import json, re, sys
s = sys.stdin.read()
hits = re.findall(r'[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@\"]+:[^/\s@\"]+@', s)
print('  FAIL: credential-shaped URL in image config' if hits else '  config: no inline credentials')
sys.exit(1 if hits else 0)
" || FAIL=1
}

if [ "${1:-}" = "--all" ]; then
  readarray -t refs < <(python3 - "$HERE/matrix.yaml" "$REGISTRY" <<'PY'
import sys, yaml
m = yaml.safe_load(open(sys.argv[1]))
for it in m["items"]:
    if it.get("builder") == "host-env":
        continue  # not a container image; build/host-envs verifies on a Mac
    print("%s:%s-%s" % (sys.argv[2], it["id"], it.get("revision", "r1")))
PY
)
  for r in "${refs[@]}"; do verify_one "$r"; done
else
  [ $# -gt 0 ] || { echo "usage: $0 <image-ref>... | --all" >&2; exit 2; }
  for ref in "$@"; do verify_one "$ref"; done
fi

echo
[ $FAIL -eq 0 ] && echo "verify: ALL OK" || echo "verify: FAILURES PRESENT"
exit $FAIL
