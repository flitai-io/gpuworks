"""Deterministic owned catalog package; configuration only, never deployed.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import gzip
import io
import re
import tarfile


def catalog_package(model_id, spec_name, title):
    if not re.fullmatch(r"e2e-[a-z0-9-]+/template", model_id):
        raise ValueError("fixture model must be an owned lower-case e2e identity")
    if any(not re.fullmatch(r"e2e-[A-Za-z0-9-]+", value) for value in (spec_name, title)):
        raise ValueError("fixture names must contain only e2e-safe text")
    model = f'''model_id: {model_id}
display_title: {title}
input_modalities: [text]
architecture: Qwen2ForCausalLM
context_length: 4096
parameter_count: 4096
readme_en: README.en.md
specs:
  - name: {spec_name}
    display_name_zh_cn: 模板 · BF16
    display_name_en: Template · BF16
    scenario: balanced
    engine: vllm
    image: gpuworks/fake-engine:test
    weights:
      quantization: fp16
      sources:
        - huggingface:
            repo_id: {model_id}
            revision: main
    weight_bytes: 1048576
    gpus:
      vendor: nvidia
      name: A100
      display_target: nvidia
    args: ["--max-model-len", "4096"]
    api:
      health_endpoint: /health
      operations:
        - type: openai.chat
          endpoint: /v1/chat/completions
          mode: stream
'''
    files = {"manifest.yaml": "schema_version: 1\npublished_at: 2026-09-01T00:00:00Z\nmodels: 1\nspecs: 1\n",
             f"models/{model_id}/model.yaml": model,
             f"models/{model_id}/README.en.md": f"# {title}\n\nOwned E2E configuration fixture. Do not deploy.\n"}
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for path, content in sorted(files.items()):
            data = content.encode()
            info = tarfile.TarInfo(path)
            info.size, info.mode, info.mtime = len(data), 0o644, 0
            archive.addfile(info, io.BytesIO(data))
    return gzip.compress(buffer.getvalue(), mtime=0)
