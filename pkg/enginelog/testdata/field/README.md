Engine output captured on real nodes on 2026-11-19/20 (GWKS-354, GWKS-373,
GWKS-420), lines cut short and the argument echo removed. Timestamps are the
nodes' own.

- sglang_ascend_nerdctl_schedulers_crashed.txt: gemma-3-27b-it tp=4 on 910B4;
  every scheduler raised 37 s after the HTTP server came up, the server kept
  answering its health check as not ready. Trimmed to the start, the first
  scheduler's exception and the end.
- sglang_ascend_nerdctl_healthy.txt: the same model with graph mode off.
- sglang_ascend_udocker_tp4_starved.txt: the same model under udocker; the
  PRoot tracer was saturated, the log gained a line every few minutes for 35
  minutes and no weights were loaded.
- vllm_ascend_udocker_tp4_starved.txt: Qwen3.8-27B TP=4 under udocker, up to
  the last line before its two-hour start timeout.
- vllm_ascend_udocker_tp2_healthy.txt: Qwen3-0.6B TP=2 on the same node,
  running after 7.6 minutes.
- vllm_hygon_docker_healthy.txt: qwen3.5-35b on K100_AI, 17.5 minutes from
  start to running.

Captured on real nodes on 2026-11-23/25 from gpuworks-runner CUDA images
(GWKS-505), cut to the start, the engine's exception and the server's
reaction:

- sglang_cuda_docker_model_attribute.txt: MiniCPMV4_6 on one H20; sglang
  0.5.23 reads a `shared_experts_config` the model class does not define
  while capturing its CUDA graphs, and the server reports the child's signal
  at launch. The same model ran under 0.5.21 on the same node.
- sglang_cuda_docker_warmup_oom.txt: a 24 GB card at the platform's
  `--mem-fraction-static 0.88`; the warmup request finds no memory and the
  server reports the signal it logs once it serves.
- sglang_cuda_docker_capture_assert.txt: Qwen3_5 at tp=4 on four 24 GB
  cards; the breakable prefill CUDA-graph capture runs the card out of
  memory and PyTorch's allocator asserts. Trimmed to the start, the last
  captures, one scheduler's traceback and the server's reaction.
