#!/usr/bin/env bash
# Debug-level decomposition on a model where the reserve is large.
#
# The 7B/8B runs hold back ~1.1-1.24 GiB while their --enforce-eager twins hold
# back ~0.30 GiB, and no "CUDA graph memory profiling" line is logged for either.
# That leaves vLLM's own printout as the only way to say which term moved.
# Device 1 only; device 0 is running the sweep.
set -u
cd /tmp/calib
mkdir -p logs-dbg

IMAGE=gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4

run() { # tag model util [extra...]
  local tag="$1" model="$2" util="$3"; shift 3
  local name="dbgprobe-$tag" log="/tmp/calib/logs-dbg/$tag.log"
  docker rm -f "$name" >/dev/null 2>&1
  docker run -d --name "$name" --gpus "device=1" \
    -v /opt/gpuworks/models:/models -v /tmp/calib:/calib \
    -v /tmp/calib/torchcache:/root/.cache/vllm \
    -e VLLM_LOGGING_LEVEL=DEBUG \
    --entrypoint vllm "$IMAGE" serve "$model" \
    --load-format dummy --gpu-memory-utilization "$util" --max-model-len 4096 \
    --port 19502 "$@" >/dev/null
  local deadline=$(( $(date +%s) + 1200 ))
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

run dbg7b-u90 /models/modelscope/Qwen2.5-7B-Instruct 0.9
echo "### debug probe 7b done $(date +%T)"
