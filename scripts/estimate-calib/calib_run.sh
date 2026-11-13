#!/usr/bin/env bash
# One vLLM dummy-loader measurement on gpu3.
#
# Hardened against the failure that cost a whole sweep: a runner killed mid-poll
# leaves its container holding the GPU, and every later run then dies with
# "Free memory ... less than desired GPU memory utilization". Runs on device 0
# are sequential, so sweeping any leftover calib container first is safe and
# makes the sweep self-healing.
#
#   usage: calib_run.sh <tag> <model_path_in_container> <gpu> <util> <maxlen> [extra vllm args...]
set -u

TAG="$1"; MODEL="$2"; GPU="$3"; UTIL="$4"; MAXLEN="$5"; shift 5

IMAGE="${IMAGE:-gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4}"
OUTDIR="${OUTDIR:-/tmp/calib/logs}"
RUN_TIMEOUT="${RUN_TIMEOUT:-900}"
NAME="calib-$TAG"

mkdir -p "$OUTDIR"
LOG="$OUTDIR/$TAG.log"
PORT=$(( 18000 + (RANDOM % 2000) ))

for stale in $(docker ps -aq --filter "name=calib-" 2>/dev/null); do
  docker rm -f "$stale" >/dev/null 2>&1
done

EXTRA_ENV="${EXTRA_ENV:-}"

docker run -d --name "$NAME" --gpus "device=$GPU" \
  -v /opt/gpuworks/models:/models -v /tmp/calib:/calib \
  -v /tmp/calib/torchcache:/root/.cache/vllm \
  ${EXTRA_ENV} \
  --entrypoint vllm "$IMAGE" serve "$MODEL" \
  --load-format dummy --gpu-memory-utilization "$UTIL" --max-model-len "$MAXLEN" \
  --port "$PORT" "$@" >/dev/null

# Wait for a terminal marker: vLLM printing the KV size, a startup failure, or
# the container exiting. Silence is not success.
deadline=$(( $(date +%s) + RUN_TIMEOUT ))
while :; do
  if ! docker ps --format '{{.Names}}' | grep -qx "$NAME"; then break; fi
  docker logs "$NAME" > "$LOG" 2>&1
  if grep -qE "GPU KV cache size|EngineCore failed|ValueError|RuntimeError|Traceback \(most recent" "$LOG"; then
    sleep 3
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "TIMEOUT after ${RUN_TIMEOUT}s" >> "$LOG"
    break
  fi
  sleep 2
done

docker logs "$NAME" > "$LOG" 2>&1
docker rm -f "$NAME" >/dev/null 2>&1
echo "log: $LOG"
