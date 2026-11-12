#!/usr/bin/env bash
# 断言镜像具备 matrix.yaml 里 requires 声明的组件。缺一项即退出非零。
#
# 这是「requires 的含义」的唯一实现：构建末尾跑一次（装不上就别出镜像），
# verify.sh 对成品再跑一次（registry 里的东西和当时构建的是不是一回事）。
#
# requires 取值：engine engine_import omni omni_no_mooncake omni_patched indextts2_frontend voxcpm decord moviepy mooncake
# diffusion overlays audit hcu_patches ppu_pd（见 matrix.yaml 顶部说明）。
#
# 用法：REQUIRES="engine omni mooncake" ENGINE=vllm [ENGINE_VERSION=0.30.0] \
#         bash assert_requires.sh
#
# 设计取舍：这里只做「组件在不在、能不能 import」的离线判定，**不等于** GPU 验收。
# 真机 serving/chat 由 artifacts/work/2026-10-03-runner-images/evidence/r1-verify.md 的真机用例负责。
set -uo pipefail

REQUIRES="${REQUIRES:-}"
ENGINE="${ENGINE:-}"
ENGINE_VERSION="${ENGINE_VERSION:-}"
FAILED=()

# 构建/离线验证容器里没有宿主驱动库：关掉 torch 的设备后端自动加载，否则 NPU/DCU 栈上的
# 任何 `import torch...`（含 diffusers、vllm）都会因后端扩展加载失败而报错，把「环境没有
# 驱动」误报成「镜像有缺陷」。真机上是否能加载由真机验证负责。
export TORCH_DEVICE_BACKEND_AUTOLOAD=0
py() { python3 -c "$1" >/dev/null 2>&1; }
dist_version() {
    python3 - "$1" <<'PY' 2>/dev/null
import importlib.metadata as m, sys
try:
    print(m.version(sys.argv[1]))
except Exception:
    sys.exit(1)
PY
}

# engine：引擎本体「装对了」—— 发行包元数据在、版本对、模块能被定位。
#
# 刻意**不**在这里强制 import：部分后端的引擎 import 需要宿主驱动库，构建容器里
# 不可能满足。实测 dtk 档裸跑 `import vllm` 必然失败：
#   ImportError: librocm_smi64.so.2: cannot open shared object file
# 该库来自宿主的 /opt/hyhal（部署时挂进容器，见 artifacts/work/2026-10-03-runner-images/evidence/r1-verify.md §3）。
# 把这种环境约束当成「镜像有缺陷」会导致该档根本构建不出来；反过来，为了让构建
# 通过就把断言放宽成「WARN」，等于声明了组件却从不校验，是假绿。所以拆成两个要求：
#   engine        —— 离线可判定的部分（元数据 + 模块可定位），所有档都要求
#   engine_import —— 真的 import 成功，只给「离线 import 本就该成功」的档声明
# 其余档的 import 由真机验证覆盖（真机用例会带上驱动挂载）。
check_engine() {
    local pkg="${ENGINE}"
    [ "${ENGINE}" = "vllm" ] || [ "${ENGINE}" = "sglang" ] \
        || { FAILED+=("engine: unknown ENGINE='${ENGINE}'"); return; }

    # 1Cat-vLLM（V100 档）：发行包叫 1cat-vllm、版本是它自己的，id 里写作 1cat<版本>；
    # 模块名与 console script 仍是 vllm。
    local dist="${pkg}" want="${ENGINE_VERSION}"
    if [ "${pkg}" = "vllm" ] && [[ "${ENGINE_VERSION}" == 1cat* ]]; then
        dist="1cat-vllm"
        want="${ENGINE_VERSION#1cat}"
    fi
    local found; found="$(dist_version "${dist}" || true)"
    if [ -z "${found}" ]; then
        FAILED+=("engine: no dist metadata for ${dist}")
        return
    fi
    if [ "${pkg}" = "vllm" ] && [ -n "${want}" ] && [ "${found%%+*}" != "${want}" ]; then
        FAILED+=("engine: ${dist} ${found} != expected ${want}")
    fi
    # find_spec 只定位模块，不执行它的 __init__（所以不会碰宿主驱动库）
    py "import importlib.util, sys; sys.exit(0 if importlib.util.find_spec('${pkg}') else 1)" \
        || FAILED+=("engine: module ${pkg} not importable-by-path")
    echo "  engine: ${pkg} ${found} (metadata+locatable)"
    # 顺带报告 import 的实际情况，便于判断某档能否升级到 engine_import
    if py "import ${pkg}"; then
        echo "  engine: import ${pkg} also succeeds offline"
    else
        echo "  engine: import ${pkg} fails offline (deferred to on-machine verification)"
    fi
}

# engine_import：真的 import 成功。只给离线 import 本就该成功的档声明。
check_engine_import() {
    py "import ${ENGINE}" \
        || FAILED+=("engine_import: import ${ENGINE} failed offline (若该后端的 import 依赖宿主驱动库，就不该声明 engine_import)")
    # `import vllm` 不会碰 vllm serve 在 CLI 阶段才导入的音频栈：cu129 的 v0.31.0 base 里 torchcodec 是 cu13 版，
    # `import vllm` 照样成功，但 `vllm serve` 一启动就 OSError(libnvrtc.so.13)（2026-10-06，164 上实测）。
    # 这个导入不需要 GPU，离线即可判定。torchcodec 也单独断言：vllm 把它的导入改成惰性之后，
    # 上面那条会先于缺陷失效，而 torchcodec 坏了音频输入照样不可用。
    if [ "${ENGINE}" = "vllm" ]; then
        py "import vllm.multimodal.media.audio" \
            || FAILED+=("engine_import: import vllm.multimodal.media.audio failed (vllm serve 的 CLI 阶段会触发；多半是 torchcodec 与 CUDA 版本不匹配)")
        if [ -n "$(dist_version torchcodec || true)" ]; then
            py "import torchcodec" \
                || FAILED+=("engine_import: torchcodec $(dist_version torchcodec) is installed but import torchcodec failed (CUDA 版本与 base 不匹配？)")
        fi
    fi
}

# omni / mooncake 都带 native 扩展，离线 import 可能因缺宿主驱动库而失败
# （与 engine 同理），所以硬断言只到「装了且能被定位」，import 结果仅作报告。
# opencv 的几个发行包（opencv-python、opencv-python-headless …）共用 cv2/ 目录。
# 不能要求「只许一份」：vllm 依赖 opencv-python-headless，而 omni 栈里的 retinaface-py、
# cosmos-guardrail 依赖 opencv-python，收敛成一份会让 pip check 报缺依赖（2026-10-06 实测，
# 审计门拦下了构建）。所以要求的是「并存就必须同版本」：cv2/ 里的文件由谁写入都一样。
check_opencv_consistent() {
    local out
    out="$(python3 -c "
import importlib.metadata as m
v = {(d.metadata['Name'] or '').lower(): d.version for d in m.distributions()
     if (d.metadata['Name'] or '').lower().startswith('opencv-')}
print(','.join(sorted(set(v.values()))) if len(set(v.values())) > 1 else '')
" 2>/dev/null || true)"
    [ -z "${out}" ] || FAILED+=("opencv: coexisting opencv-* distributions have different versions (${out}); cv2/ is shared")
}

# 运行期环境里不许带厂商内网的包源/地址（base 继承下来的 ENV 不会被构建仓的 ARG 覆盖）。
check_no_vendor_internal_env() {
    local hit
    hit="$(env | grep -i -E 't-head\.cn|eng\.t-head|zstack\.(io|ai)|sourcefind' | sed -E 's/=.*//' | tr '\n' ' ' || true)"
    [ -z "${hit}" ] || FAILED+=("env: runtime environment points at vendor-internal hosts: ${hit}")
    # 登录 shell 会 source /etc/profile.d/*，厂商脚本可能把清掉的值又 export 回来
    hit="$(bash -lc 'env' 2>/dev/null | grep -i -E 't-head\.cn|eng\.t-head|zstack\.(io|ai)|sourcefind' | sed -E 's/=.*//' | tr '\n' ' ' || true)"
    [ -z "${hit}" ] || FAILED+=("env: login-shell environment points at vendor-internal hosts: ${hit}")
}

# pip uninstall 只删文件、不删空目录，卸干净的包会在 site-packages 留下一棵只有目录的
# 残骸；PEP 420 让这棵残骸成为可导入的命名空间包，把真正该装的包遮掉——`import X` 成功，
# 但 X 没有任何属性。
#
# 为什么必须单独断言：这类缺陷的典型症状是 torch 加载设备后端时抛 AttributeError，而本脚本
# 开头的 TORCH_DEVICE_BACKEND_AUTOLOAD=0 恰好把那条链屏蔽掉，engine_import 发现不了。
# 2026-10-07：cann9.1-310p 档的 base 卸掉 triton-ascend 后留下 4 个空目录，真机上原样
# `import torch` 直接失败，而构建期 gate 全绿（artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md）。
check_no_stub_pkg_dirs() {
    local bad
    bad="$(python3 - <<'PY' 2>/dev/null || true
import os, sysconfig

roots = []
for key in ("purelib", "platlib"):
    path = sysconfig.get_paths().get(key)
    if path and os.path.isdir(path) and path not in roots:
        roots.append(path)

bad = []
for root in roots:
    for name in sorted(os.listdir(root)):
        target = os.path.join(root, name)
        if not os.path.isdir(target) or os.path.islink(target):
            continue
        if name == "__pycache__" or name.endswith((".dist-info", ".egg-info")):
            continue
        files = 0
        for _, dirnames, filenames in os.walk(target):
            # 只剩 __pycache__ 里的 .pyc 同样是残骸：没有源文件就导不出可用的包。
            dirnames[:] = [d for d in dirnames if d != "__pycache__"]
            files += len(filenames)
            if files:
                break
        if not files:
            bad.append(name)
print(" ".join(bad))
PY
)"
    [ -z "${bad}" ] || FAILED+=("site-packages: 目录树里没有任何文件，是 pip uninstall 的残骸，PEP 420 会把它变成遮蔽真包的命名空间包: ${bad}")
}

check_omni() {
    check_opencv_consistent
    local v; v="$(dist_version vllm-omni || true)"
    [ -n "${v}" ] || { FAILED+=("omni: vllm-omni has no dist metadata"); return; }
    # 声明了版本（OMNI_VERSION，可选）就必须与实际安装的一致，避免自描述写 null/写错
    if [ -n "${OMNI_VERSION:-}" ] && [ "${v%%+*}" != "${OMNI_VERSION}" ]; then
        FAILED+=("omni: vllm-omni ${v} != declared ${OMNI_VERSION}")
    fi
    py "import importlib.util, sys; sys.exit(0 if importlib.util.find_spec('vllm_omni') else 1)" \
        || { FAILED+=("omni: module vllm_omni not importable-by-path"); return; }
    if py "import vllm_omni"; then
        echo "  omni: vllm-omni ${v} (import ok)"
    else
        echo "  omni: vllm-omni ${v} (present; import deferred to on-machine verification)"
    fi
}

check_mooncake() {
    # 允许三种形态：plain / cuda13 改名包 / npu 变体。
    local v="" name=""
    for n in mooncake-transfer-engine mooncake-transfer-engine-cuda13 mooncake-transfer-engine-npu; do
        v="$(dist_version "${n}" || true)"
        [ -n "${v}" ] && { name="${n}"; break; }
    done
    if [ -z "${v}" ]; then
        FAILED+=("mooncake: no mooncake-transfer-engine* dist present")
        return
    fi
    if py "import mooncake"; then
        echo "  mooncake: ${name} ${v} (import ok)"
    else
        echo "  mooncake: ${name} ${v} (present; import deferred to on-machine verification)"
    fi
}

# diffusion：依赖「装对了」。import diffusers 会连带 import torch，dtk 档在构建容器里
# 必然因缺 librocm_smi64.so.2（宿主 /opt/hyhal）失败，所以与 engine 同理：元数据 + 模块可
# 定位是硬要求，真 import 只报告（由真机验证覆盖）。
check_diffusion() {
    local mod
    check_opencv_consistent
    for mod in diffusers cv2; do
        py "import importlib.util, sys; sys.exit(0 if importlib.util.find_spec('${mod}') else 1)" \
            || FAILED+=("diffusion: module ${mod} not found")
    done
    [ -n "$(dist_version diffusers || true)" ] || FAILED+=("diffusion: no dist metadata for diffusers")
    if py "import diffusers, cv2"; then
        echo "  diffusion: import diffusers, cv2 also succeeds offline"
    else
        echo "  diffusion: import diffusers/cv2 fails offline (deferred to on-machine verification)"
    fi
}

# omni_no_mooncake：昇腾 omni 档的镜像里不许有 pip 包 mooncake-transfer-engine-npu。
#
# 根因（2026-10-06，.32 真容器实测）：quay.io/ascend/vllm-omni 从 v0.24.0 起带这个包（v0.23.0rc1
# 没有）；它与 vllm_omni 同进程加载时（mooncake/*.so、libcann_hixl.so、libllm_datadist.so、
# libibverbs）触发 glibc 堆损坏——`import vllm_omni` / `vllm --help` 退出阶段
# `corrupted size vs. prev_size`，vLLM 的模型架构内省子进程 SIGABRT，Ming-omni-tts 与
# Qwen3-TTS 起不来。单独 import mooncake 不崩。卸掉后 import 5/5 干净、Qwen3-TTS 出音频。
# 断言的意义是换 base 时它悄悄回来会让构建失败，而不是等到真机才发现。
check_omni_no_mooncake() {
    local v bad=0 where
    v="$(dist_version mooncake-transfer-engine-npu || true)"
    if [ -n "${v}" ]; then
        bad=1
        FAILED+=("omni_no_mooncake: mooncake-transfer-engine-npu ${v} is installed (omni 档里它与 vllm_omni 同进程加载会堆损坏，必须卸掉)")
    fi
    # pip 包被改名/拆分后，模块仍会落在 python 的 site-packages 里，同样会被 vllm_omni 的进程加载到。
    # 注意不能要求「全盘没有 mooncake 模块」：v0.23.0rc1 的 CANN 9.0 工具链自带一份
    # （.../ascend-toolkit/latest/python/site-packages/mooncake，经 PYTHONPATH 可见），原件在它之上跑
    # Qwen3-TTS 无崩溃；崩溃的是 pip 装的那份。所以只拒绝落在 python 自己的 purelib/platlib 里的。
    where="$(python3 - <<'PY' 2>/dev/null
import importlib.util, os, sys, sysconfig
s = importlib.util.find_spec("mooncake")
if s and s.origin:
    o = os.path.realpath(s.origin)
    roots = {os.path.realpath(sysconfig.get_paths()[k]) for k in ("purelib", "platlib")}
    if any(o.startswith(r + os.sep) for r in roots):
        print(o)
PY
)"
    if [ -n "${where}" ]; then
        bad=1
        FAILED+=("omni_no_mooncake: module mooncake is installed in python site-packages (${where})")
    fi
    [ "${bad}" = 1 ] || echo "  omni_no_mooncake: mooncake-transfer-engine-npu absent, no pip-installed mooncake module"
}

# omni_patched：vllm_omni 的补丁已严格应用（清单里无 failed，且 Ming 的 NPU 回退补丁在文件里）。
check_omni_patched() {
    local m=/opt/gpuworks/patches-omni.json
    [ -s "${m}" ] || { FAILED+=("omni_patched: ${m} missing or empty"); return; }
    python3 - "${m}" <<'PY' || FAILED+=("omni_patched: patches-omni.json has failed entries or no applied/skipped patch")
import json, sys
d = json.load(open(sys.argv[1]))
ok = not d.get("failed") and (d.get("applied") or d.get("skipped"))
sys.exit(0 if ok else 1)
PY
    python3 - <<'PY' || FAILED+=("omni_patched: Ming audio_dsp NPU fallback (_fold_cpu / CPU hann_window) not present in vllm_omni")
import importlib.util, os, sys
s = importlib.util.find_spec("vllm_omni")
f = os.path.join(os.path.dirname(s.origin), "model_executor/models/common/ming/audio_dsp.py")
t = open(f).read()
sys.exit(0 if "_fold_cpu" in t and 'hann_window(win_length, device="cpu")' in t else 1)
PY
    echo "  omni_patched: $(tr -d '\n ' < "${m}")"
}

check_overlays() {
    local d=/opt/gpuworks/overlays
    [ -d "${d}" ] || { FAILED+=("overlays: ${d} missing"); return; }
    [ -n "$(ls -A "${d}" 2>/dev/null)" ] || FAILED+=("overlays: ${d} is empty")
}

check_audit() {
    local f=/opt/gpuworks/components.json
    [ -s "${f}" ] || { FAILED+=("audit: ${f} missing or empty"); return; }
    python3 - "${f}" <<'PY' || FAILED+=("audit: components.json status is not PASS")
import json, sys
d = json.load(open(sys.argv[1]))
sys.exit(0 if d.get("status") == "PASS" else 1)
PY
}

# indextts2_frontend：IndexTTS 2.5 的文本前端在位。fugashi 能用 unidic-lite 的词典分词；
# x86_64 上中英文规范化 tn（WeTextProcessing）能 import。arm64 上 pynini 没有 cp312 wheel，
# 本档不装，只检查分词。不构建 FST（首次请求时才编译，构建期没必要也太慢）。
check_indextts2_frontend() {
    py "import fugashi; assert fugashi.Tagger().parse('東京')" \
        || FAILED+=("indextts2_frontend: fugashi with unidic-lite cannot tokenize")
    if [ "$(uname -m)" = x86_64 ]; then
        py "import tn.chinese.normalizer, tn.english.normalizer" \
            || FAILED+=("indextts2_frontend: tn (WeTextProcessing) normalizers cannot be imported")
        echo "  indextts2_frontend: fugashi+unidic-lite, tn normalizers"
    else
        echo "  indextts2_frontend: fugashi+unidic-lite (tn not required on $(uname -m))"
    fi
}

# voxcpm：VoxCPM2 talker 依赖的原生 voxcpm 包在位，且 vllm-omni 自己的查找路径能找到它。
# 走 vllm-omni 的 import_voxcpm2_core 而不是只 import voxcpm：后者过了、前者没过的情形
# （比如装进了 vllm-omni 看不到的目录）一样起不来。voxcpm 以 --no-deps 安装，这一步同时
# 证明它的推理路径不需要那些没装的声明依赖（gradio、funasr、modelscope 等）。
check_voxcpm() {
    py "from vllm_omni.model_executor.models.voxcpm2.voxcpm2_import_utils import import_voxcpm2_core; import_voxcpm2_core()" \
        || FAILED+=("voxcpm: vllm-omni cannot import voxcpm.core.VoxCPM")
}

# decord：HF 动态模块（ERNIE-4.5-VL）要求的视频解码库。由 decord2 提供同名模块，
# 除了能 import，还要真能解出帧：用 ffmpeg 现做一段 1 秒的合成视频读回来。
check_decord() {
    local work; work="$(mktemp -d)"
    if ffmpeg -loglevel error -f lavfi -i testsrc=size=64x48:rate=5 -t 1 -pix_fmt yuv420p "${work}/t.mp4" \
        && python3 - "${work}/t.mp4" <<'PY' >/dev/null 2>&1
import sys
from decord import VideoReader, cpu
vr = VideoReader(sys.argv[1], ctx=cpu(0))
assert len(vr) == 5, len(vr)
assert tuple(vr.get_batch([0, 4]).shape) == (2, 48, 64, 3)
PY
    then
        echo "  decord: decoded 5 frames"
    else
        FAILED+=("decord: cannot decode a synthetic mp4")
    fi
    rm -rf "${work}"
}

# moviepy：ERNIE-4.5-VL 的动态模块直接 import moviepy。确认 import 路径存在，
# 而不是只看 pip 元数据。
check_moviepy() {
    py "import moviepy; from moviepy import ColorClip; ColorClip((1, 1), color=(0, 0, 0), duration=0.01)" \
        || FAILED+=("moviepy: cannot import and construct ColorClip")
}

check_hcu_patches() {
    command -v vllm-hcu-apply-patches >/dev/null 2>&1 \
        || FAILED+=("hcu_patches: vllm-hcu-apply-patches not on PATH")
}

check_mooncake_load_errors() {
    # vLLM 的 MooncakeConnector 把拉取失败的块上报给调度器（patches/vllm_dtk/001），
    # kv_load_failure_policy 才会生效；没有它时 decode 拿未收到的 KV 照样回答。
    # 只读源码：dtk 档在构建容器里 import vllm 会因缺宿主库失败。
    check_mooncake
    python3 - <<'PY' >/dev/null 2>&1 || FAILED+=("mooncake_load_errors: MooncakeConnector does not report failed pulls (patches/vllm_dtk/001)")
import importlib.util, pathlib
root = pathlib.Path(importlib.util.find_spec("vllm").origin).parent
src = (root / "distributed/kv_transfer/kv_connector/v1/mooncake/mooncake_connector.py").read_text()
assert src.count("def get_block_ids_with_load_errors") == 2, "connector and worker"
assert "self._invalid_block_ids.update(group)" in src
PY
}

check_mooncake_hybrid() {
    # GDN/Mamba 混合模型（Qwen3.5/3.6/3.8）的 vllm-mooncake PD（patches/vllm_dtk/002、003）：
    # 连接器按整页传混合 KV、prefill 只算 N-1，调度器在多 KV 组上处理拉取失败而不让 EngineCore 崩。
    # 只读源码，理由同上。
    check_mooncake_load_errors
    python3 - <<'PY' >/dev/null 2>&1 || FAILED+=("mooncake_hybrid: MooncakeConnector or the scheduler lacks the GDN hybrid PD patches (patches/vllm_dtk/002, 003)")
import importlib.util, pathlib
root = pathlib.Path(importlib.util.find_spec("vllm").origin).parent
src = (root / "distributed/kv_transfer/kv_connector/v1/mooncake/mooncake_connector.py").read_text()
assert "def _register_hybrid_page_caches" in src
assert "def _truncate_mamba_request_for_prefill" in src
assert "is_mamba=self._has_mamba," in src
sched = (root / "v1/core/sched/scheduler.py").read_text()
assert "block_ids_per_group = self.kv_cache_manager.get_block_ids(req_id)" in sched
PY
}

check_ppu_pd() {
    # PPU 同机 PD（pdmode sglang-ppu-ipc）：hggc/ppu-pd 的补丁装进了 sglang，
    # 且 mooncake 可用。补丁只在 SGLANG_PPU_PD_IPC=1 时生效。
    check_mooncake
    python3 - <<'PY' >/dev/null 2>&1 || FAILED+=("ppu_pd: SGLang lacks the PPU PD patches (hggc/ppu-pd/install_ppu_pd_ipc.py)")
import pathlib, sglang
root = pathlib.Path(sglang.__file__).resolve().parent / "srt" / "disaggregation"
for name in ("ppu_ipc_registration.py", "ppu_serial_transfer.py"):
    assert (root / name).is_file(), name
assert "SGLANG_PPU_PD_IPC" in (root / "mooncake" / "conn.py").read_text()
assert "SGLANG_PPU_PD_IPC" in (root / "utils.py").read_text()
assert "ppu_serial_transfer_enabled" in (root / "prefill.py").read_text()
PY
}

[ -n "${REQUIRES}" ] || { echo "assert_requires: REQUIRES is empty" >&2; exit 2; }

for item in ${REQUIRES}; do
    case "${item}" in
        engine)        check_engine ;;
        engine_import) check_engine_import ;;
        omni)         check_omni ;;
        omni_no_mooncake) check_omni_no_mooncake ;;
        omni_patched) check_omni_patched ;;
        indextts2_frontend) check_indextts2_frontend ;;
        voxcpm)       check_voxcpm ;;
        decord)       check_decord ;;
        moviepy)      check_moviepy ;;
        mooncake)     check_mooncake ;;
        diffusion)    check_diffusion ;;
        overlays)     check_overlays ;;
        audit)        check_audit ;;
        hcu_patches)  check_hcu_patches ;;
        ppu_pd)       check_ppu_pd ;;
        mooncake_load_errors) check_mooncake_load_errors ;;
        mooncake_hybrid) check_mooncake_hybrid ;;
        *) FAILED+=("unknown requirement '${item}'") ;;
    esac
done

check_no_vendor_internal_env
check_no_stub_pkg_dirs

if [ ${#FAILED[@]} -gt 0 ]; then
    echo "=== assert_requires FAILED (engine=${ENGINE} requires='${REQUIRES}')" >&2
    for f in "${FAILED[@]}"; do echo "  - ${f}" >&2; done
    exit 1
fi
echo "assert_requires OK (engine=${ENGINE} requires='${REQUIRES}')"
