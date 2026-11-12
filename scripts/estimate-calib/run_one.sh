#!/usr/bin/env bash
# One vLLM dummy-loader measurement on a real GPU.
#
#   usage: run_one.sh <tag> <model_path_in_container> <gpu> <util> <maxlen> [extra vllm args...]
#
# The model directory must already exist under /opt/gpuworks/models on the host
# (mounted at /models); with --load-format dummy only config.json + tokenizer
# are actually needed, so a config-only directory is enough.
#
# Writes the raw container log to $OUTDIR/<tag>.log and never touches the
# gpuworks agent or its containers.
set -u

TAG="$1"; MODEL="$2"; GPU="$3"; UTIL="$4"; MAXLEN="$5"; shift 5

IMAGE="${IMAGE:-gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4}"
OUTDIR="${OUTDIR:-/tmp/calib/logs}"
BLOCK_SIZE="${BLOCK_SIZE:-}"
RUN_TIMEOUT="${RUN_TIMEOUT:-900}"
NAME="calib-$TAG"

mkdir -p "$OUTDIR"
LOG="$OUTDIR/$TAG.log"
PORT=$(( 18000 + (RANDOM % 2000) ))

# A leftover container from a killed run would collide on the name.
docker rm -f "$NAME" >/dev/null 2>&1

EXTRA=()
if [ -n "$BLOCK_SIZE" ]; then EXTRA+=(--block-size "$BLOCK_SIZE"); fi

# EXTRA_ENV passes -e pairs through, e.g. EXTRA_ENV="-e VLLM_LOGGING_LEVEL=DEBUG".
docker run -d --name "$NAME" --gpus "device=$GPU" \
  -v /opt/gpuworks/models:/models -v /tmp/calib:/calib \
  -v /tmp/calib/torchcache:/root/.cache/vllm \
  ${EXTRA_ENV:-} \
  --entrypoint vllm "$IMAGE" serve "$MODEL" \
  --load-format dummy --gpu-memory-utilization "$UTIL" --max-model-len "$MAXLEN" \
  --port "$PORT" "${EXTRA[@]}" "$@" >/dev/null

# Wait for a terminal marker: vLLM printing the KV size, a startup failure, or
# the container exiting. Silence is not success - poll until one of them lands.
deadline=$(( $(date +%s) + RUN_TIMEOUT ))
while :; do
  if ! docker ps --format '{{.Names}}' | grep -qx "$NAME"; then break; fi
  docker logs "$NAME" > "$LOG" 2>&1
  if grep -qE "GPU KV cache size|Available KV cache memory|Application startup complete|EngineCore failed|ValueError|RuntimeError|Traceback \(most recent" "$LOG"; then
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
