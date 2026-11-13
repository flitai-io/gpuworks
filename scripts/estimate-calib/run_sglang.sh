#!/usr/bin/env bash
# One SGLang dummy-loader measurement. Mirrors run_one.sh for the vLLM side so
# the two engines' static-memory accounting can be compared on the same model.
#
#   usage: run_sglang.sh <tag> <model_path_in_container> <gpu> <mem_fraction> <context_len> [extra args...]
set -u

TAG="$1"; MODEL="$2"; GPU="$3"; FRAC="$4"; CTX="$5"; shift 5

IMAGE="${IMAGE:-gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r3}"
OUTDIR="${OUTDIR:-/tmp/calib/logs-sglang}"
RUN_TIMEOUT="${RUN_TIMEOUT:-600}"
NAME="calib-sg-$TAG"

mkdir -p "$OUTDIR"
LOG="$OUTDIR/$TAG.log"
PORT=$(( 21000 + (RANDOM % 2000) ))

docker rm -f "$NAME" >/dev/null 2>&1

docker run -d --name "$NAME" --gpus "device=$GPU" --shm-size 16g \
  -v /opt/gpuworks/models:/models -v /tmp/calib:/calib \
  -v /tmp/calib/torchcache-sg:/root/.cache \
  --entrypoint python3 "$IMAGE" -m sglang.launch_server \
  --model-path "$MODEL" --load-format dummy \
  --mem-fraction-static "$FRAC" --context-length "$CTX" \
  --host 0.0.0.0 --port "$PORT" "$@" >/dev/null

deadline=$(( $(date +%s) + RUN_TIMEOUT ))
while :; do
  if ! docker ps --format '{{.Names}}' | grep -qx "$NAME"; then break; fi
  docker logs "$NAME" > "$LOG" 2>&1
  if grep -qE "KV Cache is allocated|The server is fired up|Capture cuda graph|Error|Traceback \(most recent" "$LOG"; then
    sleep 5
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
