#!/usr/bin/env bash
# Two debug-level runs on device 1 that differ only in gpu_memory_utilization.
#
# The measured non-weight reserve moved with util (0.99 GiB at 0.9 vs 0.71 GiB
# at 0.5) on a model whose activation footprint cannot explain the difference.
# vLLM logs its own decomposition of that reserve at DEBUG level, which is the
# only way to attribute it. Uses its own container names so it never trips the
# janitor in calib_run.sh, which would remove the sweep running on device 0.
set -u
cd /tmp/calib
mkdir -p logs-dbg

run() { # tag util
  local tag="$1" util="$2"
  local name="dbgprobe-$tag" log="/tmp/calib/logs-dbg/$tag.log"
  docker rm -f "$name" >/dev/null 2>&1
  docker run -d --name "$name" --gpus "device=1" \
    -v /opt/gpuworks/models:/models -v /tmp/calib:/calib \
    -v /tmp/calib/torchcache:/root/.cache/vllm \
    -e VLLM_LOGGING_LEVEL=DEBUG \
    --entrypoint vllm gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4 \
    serve /models/modelscope/Qwen_Qwen3-0.6B-default \
    --load-format dummy --gpu-memory-utilization "$util" --max-model-len 4096 \
    --port 19501 >/dev/null
  local deadline=$(( $(date +%s) + 900 ))
  while :; do
    if ! docker ps --format '{{.Names}}' | grep -qx "$name"; then break; fi
    docker logs "$name" > "$log" 2>&1
    if grep -qE "GPU KV cache size|EngineCore failed|ValueError|RuntimeError|Traceback \(most recent" "$log"; then
      sleep 3; break
    fi
    [ "$(date +%s)" -ge "$deadline" ] && { echo TIMEOUT >> "$log"; break; }
    sleep 2
  done
  docker logs "$name" > "$log" 2>&1
  docker rm -f "$name" >/dev/null 2>&1
  echo "log: $log"
}

run dbg-u90 0.9
run dbg-u50 0.5
echo "### debug probe done $(date +%T)"
