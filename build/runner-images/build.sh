#!/usr/bin/env bash
# gpuworks-runner 构建驱动：按 matrix.yaml 逐项 buildx 构建并推送。
#
# 用法（在构建机上，本脚本位于 build/ 目录内执行）：
#   ./build.sh --list
#   ./build.sh --only cuda13.0-vllm0.30.0
#   ./build.sh --backend cann --revision-only r2      # 只构建 r2 的档
#   ./prepare-base.sh <vendor-base> <clean-base>     # 厂商 base 先压掉 history
#   ./build.sh --only cuda13.0-sglang0.5.21 --no-push --platform linux/amd64  # 迭代验证
#
# 环境变量：
#   REGISTRY       推送目标前缀（默认 registry.zstack.ai/gpuworks/gpuworks-runner）
#   BASE_MIRROR    把 base 的 registry 主机换成这个 mirror（厂商 registry 不可达时用；
#                  matrix.yaml 里的 base 必须始终记录可复现的上游来源，不写 mirror）
#   BUILDER        buildx builder 名（默认 gpuworks-builder）
#
# 前置：
#   docker buildx create --name gpuworks-builder --driver docker-container
#   docker run --privileged --rm tonistiigi/binfmt --install arm64   # 跨架构
#   docker login "${REGISTRY%%/*}"                                   # 推送凭据
set -euo pipefail

REGISTRY="${REGISTRY:-registry.zstack.ai/gpuworks/gpuworks-runner}"
BASE_MIRROR="${BASE_MIRROR:-}"
BUILDER="${BUILDER:-gpuworks-builder}"
HERE="$(cd "$(dirname "$0")" && pwd)"

ONLY=""; BACKEND=""; REVISION_ONLY=""; NO_PUSH=0; LIST=0; DRY=0; PLATFORM_OVERRIDE=""; MAKE_ALIAS=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --only) ONLY="$2"; shift 2 ;;
    --backend) BACKEND="$2"; shift 2 ;;
    --revision-only) REVISION_ONLY="$2"; shift 2 ;;
    --no-push) NO_PUSH=1; shift ;;
    # 裸 tag（无版次）默认**不**创建：它一旦指过去，节点和 catalog 就可能拉到还没
    # 真机验收的镜像。验收通过后再显式加 --alias 让它指向该版次。
    --alias) MAKE_ALIAS=1; shift ;;
    # 只为迭代验证用：单架构 + --no-push 才能 --load 到本地 daemon 跑 verify.sh。
    # 正式产出一律用 matrix.yaml 声明的架构，不要带这个参数。
    --platform) PLATFORM_OVERRIDE="$2"; shift 2 ;;
    --list) LIST=1; shift ;;
    --dry-run) DRY=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

command -v python3 >/dev/null || { echo "python3 required (pyyaml)"; exit 1; }

# matrix.yaml → 每行一项：id \t revision \t backend \t engine \t platforms \t base \t requires \t args(JSON)
readarray -t ITEMS < <(python3 - "$HERE/matrix.yaml" <<'PY'
import sys, json, yaml
m = yaml.safe_load(open(sys.argv[1]))
default_platforms = (m.get("defaults") or {}).get("platforms") or ["linux/amd64"]
for it in m["items"]:
    if it.get("builder") == "host-env":
        continue  # macOS host environment packages: build/host-envs/build.sh
    if it.get("builder") == "prebuilt":
        print("prebuilt %s: pinned artifact; see %s/README.md" % (it["id"], it["backend"]), file=sys.stderr)
        continue
    platforms = it.get("platforms") or default_platforms
    print("\t".join([
        it["id"], it.get("revision", "r1"), it["backend"], it["engine"],
        ",".join(platforms), it["base"],
        " ".join(it.get("requires") or []),
        json.dumps(it.get("args") or {}, ensure_ascii=False),
        "1" if it.get("base_requires_clean_history") else "0",
    ]))
PY
)

if [[ $LIST -eq 1 ]]; then
  printf '%-34s %-4s %-6s %-7s %-24s %-34s %s\n' ID REV BACKEND ENGINE PLATFORMS BASE REQUIRES
  for line in "${ITEMS[@]}"; do
    IFS=$'\t' read -r id rev be eng plat base req args needclean <<<"$line"
    printf '%-34s %-4s %-6s %-7s %-24s %-34s %s\n' "$id" "$rev" "$be" "$eng" "$plat" "${base:0:34}" "$req"
  done
  exit 0
fi

# base 的本地 mirror 映射。
#
# matrix.yaml 的 base 永远记录可复现的上游来源；厂商 registry 不可达时，用
# base-mirrors.local 把它显式映射到本地 mirror 里的实际位置（那里的仓库路径
# 常常和上游不一样）。该文件是构建机本地配置，不进仓库——见 base-mirrors.example。
declare -A BASE_MIRROR_MAP=()
if [[ -f "$HERE/base-mirrors.local" ]]; then
  while read -r upstream mirrored; do
    [[ -z "$upstream" || "$upstream" == \#* ]] && continue
    BASE_MIRROR_MAP["$upstream"]="$mirrored"
  done < "$HERE/base-mirrors.local"
  echo "loaded ${#BASE_MIRROR_MAP[@]} base mirror mapping(s) from base-mirrors.local"
fi

mirror_base() {
  local base="$1"
  if [[ -n "${BASE_MIRROR_MAP[$base]:-}" ]]; then
    echo "${BASE_MIRROR_MAP[$base]}"; return
  fi
  local first="${base%%/*}"
  local has_host=0
  [[ "$base" == *"/"* && ( "$first" == *.* || "$first" == *:* ) ]] && has_host=1
  if [[ $has_host -eq 0 && "$base" != */* ]]; then
    # 短名（如 asllm:2.0.0-vanilla）既不是上游公开镜像，也没有映射：
    # 若放任不管会被解析成 docker.io/library/<name>，拉到无关镜像或直接失败。
    echo "ERROR: base '$base' has no registry host and no base-mirrors.local entry." >&2
    echo "       厂商 tar 类 base 必须在 base-mirrors.local 里显式映射到本地 mirror。" >&2
    return 1
  fi
  if [[ -n "$BASE_MIRROR" && $has_host -eq 1 ]]; then
    echo "${BASE_MIRROR}/${base#*/}"; return
  fi
  echo "$base"
}

built=0; skipped=0
for line in "${ITEMS[@]}"; do
  IFS=$'\t' read -r id rev be eng plat base req args needclean <<<"$line"
  [[ -n "$ONLY" && "$id" != "$ONLY" ]] && continue
  [[ -n "$BACKEND" && "$be" != "$BACKEND" ]] && continue
  [[ -n "$REVISION_ONLY" && "$rev" != "$REVISION_ONLY" ]] && continue

  [[ -n "$PLATFORM_OVERRIDE" ]] && plat="$PLATFORM_OVERRIDE"
  dockerfile="$HERE/$be/Dockerfile.$eng"
  [[ -f "$dockerfile" ]] || { echo "SKIP $id: no $dockerfile"; skipped=$((skipped+1)); continue; }

  effective_base="$(mirror_base "$base")" || { echo "SKIP $id: base unmapped" >&2; exit 1; }
  # 不可变 tag 是权威引用（catalog 用它）；裸 tag 只是人用别名。
  immutable="${REGISTRY}:${id}-${rev}"
  alias_tag="${REGISTRY}:${id}"

  build_args=(--file "$dockerfile" "--platform=${plat}"
              --build-arg "BASE_IMAGE=$effective_base"
              --build-arg "IMAGE_ID=$id"
              --build-arg "IMAGE_REVISION=$rev"
              --build-arg "REQUIRES=$req"
              --provenance=false --sbom=false)
  # 构建期 pip 源（Dockerfile 的 PIP_INDEX_URL 默认阿里云）。镜像站同步不全时
  # （2026-10-10 阿里云缺 transformers 5.14.1 的元数据）用环境变量临时换源；不写进镜像。
  [[ -n "${PIP_INDEX_URL:-}" ]] && build_args+=(--build-arg "PIP_INDEX_URL=$PIP_INDEX_URL")
  while IFS= read -r kv; do
    [[ -n "$kv" ]] && build_args+=(--build-arg "$kv")
  done < <(python3 -c "
import json,sys
for k, v in json.loads(sys.argv[1]).items():
    print('%s=%s' % (k, v))
" "$args")

  # 厂商 base 的 history 可能含内网凭据，而 history 随镜像分发。这类档在构建前
  # 必须确认 effective base 已经被 prepare-base.sh 清过。
  if [[ "$needclean" == "1" && $DRY -eq 0 ]]; then
    echo "    checking base history: $effective_base"
    # 不要在这里 docker pull：checker 本来就能用 skopeo 直读 registry，而本地
    # mirror 常是明文 HTTP，docker CLI 根本拉不动（会让这一步连带整个构建失败）。
    # 厂商 base 还必须没有被夹带 ZStack/AIOS 内部组件（2026-10-05：dtk-vllm 的 base 就是这样漏过
    # 了只查凭据的清洗门）。注意：prepare-base.sh 清洗后的 base 只剩一层 history，这道检查对它
    # 看不出东西，所以**在清洗之前**的原始 base 上必须过这道门——见 prepare-base.sh。
    if ! bash "$HERE/shared/check_vendor_base.sh" "$effective_base" | sed 's/^/      /'; then
        echo "ERROR: base '$effective_base' 带有 ZStack/AIOS 痕迹，不是厂商原件。" >&2; exit 1
    fi
    if ! bash "$HERE/shared/check_clean_history.sh" "$effective_base" | sed 's/^/      /'; then
      echo "ERROR: base '$effective_base' 的 history 里含凭据形态字符串。" >&2
      echo "       先跑 ./prepare-base.sh '$effective_base' '<clean-ref>'，" >&2
      echo "       再把 base-mirrors.local 的映射指向 clean-ref。" >&2
      exit 1
    fi
  fi

  echo "==> $id rev=$rev base=$effective_base platforms=$plat requires='$req'"
  if [[ $DRY -eq 1 ]]; then
    echo "    (dry-run) would build: docker buildx build ${build_args[*]} --tag $immutable $HERE"
    continue
  fi

  if [[ $NO_PUSH -eq 1 ]]; then
    if [[ "$plat" == *","* ]]; then
      docker buildx build --builder "$BUILDER" "${build_args[@]}" --tag "$immutable" "$HERE"
    else
      docker buildx build --builder "$BUILDER" "${build_args[@]}" --load --tag "$immutable" "$HERE"
    fi
  else
    docker buildx build --builder "$BUILDER" "${build_args[@]}" --push --tag "$immutable" "$HERE"
  fi

  # 别名 tag：指向同一 manifest，不重新构建
  if [[ $NO_PUSH -eq 0 && $MAKE_ALIAS -eq 1 ]]; then
    if command -v /usr/local/bin/skopeo >/dev/null; then
      /usr/local/bin/skopeo copy --all "docker://$immutable" "docker://$alias_tag" >/dev/null
      echo "    alias $alias_tag -> $immutable"
    else
      echo "    WARN: skopeo missing, alias tag $alias_tag not created"
    fi
  elif [[ $NO_PUSH -eq 0 ]]; then
    echo "    (alias $alias_tag 未创建；验收后用 --alias 再跑一次即可)"
  fi
  built=$((built+1))
  echo "==> done $id -> $immutable"
done
echo "built=$built skipped=$skipped"
