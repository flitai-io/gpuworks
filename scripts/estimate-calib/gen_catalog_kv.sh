#!/usr/bin/env bash
# Produce the measured KV-bytes-per-token table for the whole seed catalog.
#
# The number comes from vLLM's own layout functions applied to each model's
# published config.json (kv_layout.py), which is a CPU computation: no GPU is
# involved and the model's size is irrelevant. Where a registry's config.json
# cannot be fetched, the model simply gets no value.
#
#   usage: gen_catalog_kv.sh <targets.json> <out.json>
#
# Run on a host that can reach the registry and has the vLLM image.
set -u
TARGETS="${1:-/tmp/calib/kvtargets.json}"
OUT="${2:-/tmp/calib/kvvalues.json}"
WORK=/tmp/calib/kvgen
IMAGE="${IMAGE:-gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4}"

mkdir -p "$WORK/cfg"

python3 - "$TARGETS" "$WORK" <<'PY'
import json, os, sys, urllib.request

targets, work = json.load(open(sys.argv[1])), sys.argv[2]
fetched = skipped = 0
for model_id, t in targets.items():
    if not t.get("ms") or t.get("kv_cache") is False:
        skipped += 1
        continue
    # One directory per model, which is what vLLM resolves a model from.
    d = os.path.join(work, "cfg", model_id.replace("/", "__"))
    path = os.path.join(d, "config.json")
    if os.path.exists(path):
        fetched += 1
        continue
    os.makedirs(d, exist_ok=True)
    url = ("https://modelscope.cn/api/v1/models/%s/repo?Revision=master&FilePath=config.json"
           % t["ms"])
    try:
        with urllib.request.urlopen(url, timeout=30) as r:
            body = r.read() if r.status == 200 else None
    except Exception:
        body = None
    if not body:
        os.rmdir(d) if not os.listdir(d) else None
        continue
    open(path, "wb").write(body)
    fetched += 1
print("fetched=%d skipped=%d" % (fetched, skipped), file=sys.stderr)
PY

docker run --rm -v /tmp/calib:/calib --entrypoint python3 "$IMAGE" \
  /calib/kv_layout.py --json /calib/kvvalues.json /calib/kvgen/cfg > "$OUT" 2>/tmp/calib/kvgen/err.log
echo "wrote $OUT ($(python3 -c "import json;print(len(json.load(open('$OUT'))))" 2>/dev/null || echo '?') models)"
