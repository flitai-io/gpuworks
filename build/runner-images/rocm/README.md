# ROCm runner recipes

The ROCm entries in `../matrix.yaml` own the public upstream base digests,
component versions and platform constraints; this directory is the single
implementation of ROCm preparation, repair and component auditing.

`rocm7.2.3-vllm0.30.0` is published and listed in the built-in image catalog.
Its base, the upstream `vllm/vllm-openai-rocm` image, ships kernels for gfx942
(MI300X/MI325X) and gfx1151 (Ryzen AI Max+ 395) among others; the built-in
launch specs name those two architectures. Use the [importable gfx1100 profiles](../../inference-templates/amd-gfx1100/qwen3.yaml)
for initial Radeon PRO W7900 validation with Qwen3-0.6B and Qwen3-8B. The SGLang entry is an
unpublished MI300X (gfx942) candidate with `catalog: false`.
