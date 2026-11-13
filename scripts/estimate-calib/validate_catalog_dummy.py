#!/usr/bin/env python3
"""Validate every catalog model that has an NVIDIA/vLLM template.

The validator is deliberately independent of the server database. It walks
the seed inference-template YAML, downloads the smallest useful ModelScope
file set, and starts one vLLM ``--load-format dummy`` container per model. A
failed or too-large model is recorded and the sweep continues. Results and
logs are resumable, so a 300-model catalog can be run over several sessions.

Typical use on a GPU host (the host must have Docker and /opt/gpuworks/models):

    python3 validate_catalog_dummy.py \
      --catalog-dir internal/server/inferencecatalog/seed/catalog/models \
      --models-root /opt/gpuworks/models/modelscope \
      --output /tmp/calib/catalog-vllm-validation.json

``--dry-run`` only discovers templates and does not download or start a
container. The default max model length is intentionally small: it changes
the available pool, but not the per-token layout being validated.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import re
import signal
import subprocess
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

try:
    import yaml
except Exception:  # pragma: no cover - the fallback keeps the host tool portable
    yaml = None

GIB = float(1 << 30)
DEFAULT_IMAGE = "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4"
FILES = (
    "config.json",
    "generation_config.json",
    "tokenizer.json",
    "tokenizer_config.json",
    "tokenizer.model",
    "vocab.json",
    "merges.txt",
    "special_tokens_map.json",
    "chat_template.jinja",
    "preprocessor_config.json",
    "processor_config.json",
)


def model_id_from_text(text: str) -> str:
    match = re.search(r"^model_id:\s*([^\s#]+)", text, re.MULTILINE)
    return match.group(1).strip("'\"") if match else ""


def fallback_repos(text: str) -> list[str]:
    # Model YAML uses one-key mappings such as ``modelscope:`` followed by a
    # repo_id. This fallback is only used when PyYAML is unavailable.
    repos = []
    for match in re.finditer(r"modelscope:\s*\n\s*repo_id:\s*([^\s#]+)", text):
        repo = match.group(1).strip("'\"")
        if repo not in repos:
            repos.append(repo)
    return repos


def discover(catalog_dir: Path) -> list[dict]:
    rows = []
    for path in sorted(catalog_dir.rglob("model.yaml")):
        text = path.read_text(encoding="utf-8")
        model_id = model_id_from_text(text)
        if not model_id:
            rows.append({"yaml": str(path), "status": "invalid_missing_model_id"})
            continue
        nvidia_vllm = False
        repos: list[str] = []
        if yaml is not None:
            try:
                doc = yaml.safe_load(text) or {}
                for spec in doc.get("specs") or []:
                    gpus = spec.get("gpus") or {}
                    if str(spec.get("engine", "")).lower() != "vllm":
                        continue
                    if str(gpus.get("vendor", "")).lower() != "nvidia":
                        continue
                    nvidia_vllm = True
                    for source in (spec.get("weights") or {}).get("sources") or []:
                        item = source.get("modelscope") if isinstance(source, dict) else None
                        if isinstance(item, str):
                            repo = item
                        elif isinstance(item, dict):
                            repo = item.get("repo_id", "")
                        else:
                            repo = ""
                        if repo and repo not in repos:
                            repos.append(repo)
            except Exception as exc:
                rows.append({"model_id": model_id, "yaml": str(path), "status": "invalid_yaml", "error": str(exc)})
                continue
        else:
            repos = fallback_repos(text)
            nvidia_vllm = "engine: vllm" in text and "vendor: nvidia" in text
        rows.append({
            "model_id": model_id,
            "yaml": str(path),
            "modelscope_repos": repos,
            "has_nvidia_vllm": nvidia_vllm,
        })
    return rows


def download_model(repo: str, root: Path, revision: str = "master") -> tuple[Path | None, str]:
    safe = re.sub(r"[^A-Za-z0-9_.-]+", "_", repo)
    dest = root / safe
    dest.mkdir(parents=True, exist_ok=True)
    for filename in FILES:
        target = dest / filename
        if target.exists() and target.stat().st_size > 0:
            continue
        query = urllib.parse.urlencode({"Revision": revision, "FilePath": filename})
        url = f"https://modelscope.cn/api/v1/models/{repo}/repo?{query}"
        try:
            with urllib.request.urlopen(url, timeout=30) as response:
                if response.status != 200:
                    continue
                target.write_bytes(response.read())
        except Exception:
            target.unlink(missing_ok=True)
    config = dest / "config.json"
    if not config.exists() or config.stat().st_size == 0:
        return None, "modelscope_config_unavailable"
    return dest, ""


def parse_log(path: Path) -> dict:
    text = path.read_text(encoding="utf-8", errors="replace")
    out: dict = {
        "log": str(path),
        "vllm_version": (re.search(r"version\s+([0-9][^\s]+)", text) or [None, None])[1],
        "available_kv_gib": None,
        "kv_tokens": None,
        "bytes_per_token": None,
    }
    match = re.search(r"Available KV cache memory:\s*([0-9.]+)\s*GiB", text)
    if match:
        out["available_kv_gib"] = float(match.group(1))
    match = re.search(r"GPU KV cache size:\s*([0-9,]+)\s*tokens", text)
    if match:
        out["kv_tokens"] = int(match.group(1).replace(",", ""))
    if out["available_kv_gib"] and out["kv_tokens"]:
        out["bytes_per_token"] = out["available_kv_gib"] * GIB / out["kv_tokens"]
    if out["kv_tokens"] and out["available_kv_gib"]:
        out["status"] = "measured"
    elif re.search(r"# exit_code:\s*[1-9]", text) or "# runner_error:" in text:
        out["status"] = "runner_failed"
    elif re.search(r"Traceback \(most recent call last\)|EngineCore failed|CUDA out of memory|ERROR", text):
        out["status"] = "startup_failed"
    else:
        out["status"] = "no_measurement"
    return out


def run_one(row: dict, args: argparse.Namespace, model_dir: Path, log_dir: Path) -> dict:
    slug = re.sub(r"[^A-Za-z0-9_.-]+", "_", row["model_id"])
    name = f"catalog-calib-{slug[:50]}"
    log_path = log_dir / f"{slug}.log"
    port = random.randint(18000, 19999)
    command = [
        "docker", "run", "--name", name, "--gpus", f"device={args.gpu}",
        "-v", f"{args.models_root}:/models:ro", "--entrypoint", "vllm", args.image,
        "serve", f"/models/{model_dir.name}", "--load-format", "dummy",
        "--gpu-memory-utilization", str(args.util), "--max-model-len", str(args.max_model_len),
        "--port", str(port), "--trust-remote-code",
    ]
    if args.enforce_eager:
        command.append("--enforce-eager")
    log_dir.mkdir(parents=True, exist_ok=True)
    with log_path.open("w", encoding="utf-8") as output:
        output.write("# command: " + " ".join(command) + "\n")
        output.flush()
        subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            proc = subprocess.Popen(command, stdout=output, stderr=subprocess.STDOUT, text=True)
            deadline = time.monotonic() + args.timeout
            while proc.poll() is None and time.monotonic() < deadline:
                time.sleep(1)
            if proc.poll() is None:
                output.write(f"\nTIMEOUT after {args.timeout}s\n")
                proc.send_signal(signal.SIGTERM)
                try:
                    proc.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    proc.kill()
            output.write(f"\n# exit_code: {proc.returncode}\n")
        except OSError as exc:
            output.write(f"\n# runner_error: {exc}\n")
        finally:
            subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    measured = parse_log(log_path)
    measured.update({"model_id": row["model_id"], "modelscope_repo": row.get("modelscope_repo"), "image": args.image})
    return measured


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--catalog-dir", type=Path, default=Path("internal/server/inferencecatalog/seed/catalog/models"))
    parser.add_argument("--models-root", type=Path, default=Path("/opt/gpuworks/models/modelscope"))
    parser.add_argument("--log-dir", type=Path, default=Path("/tmp/calib/catalog-logs"))
    parser.add_argument("--output", type=Path, default=Path("/tmp/calib/catalog-vllm-validation.json"))
    parser.add_argument("--image", default=DEFAULT_IMAGE)
    parser.add_argument("--gpu", default="0")
    parser.add_argument("--util", type=float, default=0.90)
    parser.add_argument("--max-model-len", type=int, default=4096)
    parser.add_argument("--timeout", type=int, default=420)
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--retry-failed", action="store_true")
    parser.add_argument("--enforce-eager", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    rows = discover(args.catalog_dir)
    if args.limit > 0:
        rows = rows[: args.limit]
    previous = {}
    if args.resume and args.output.exists():
        try:
            previous = {item.get("model_id"): item for item in json.loads(args.output.read_text()) if item.get("model_id")}
        except Exception:
            previous = {}
    results = []
    for index, row in enumerate(rows, 1):
        model_id = row.get("model_id", "")
        if not model_id:
            results.append(row)
            continue
        if not row.get("has_nvidia_vllm"):
            result = {"model_id": model_id, "status": "skipped_no_nvidia_vllm", "yaml": row.get("yaml")}
            results.append(result)
            print(f"[{index}/{len(rows)}] {model_id}: {result['status']}", flush=True)
            continue
        old = previous.get(model_id)
        if old and (old.get("status") == "measured" or (not args.retry_failed and old.get("status") in {"startup_failed", "modelscope_config_unavailable"})):
            results.append(old)
            print(f"[{index}/{len(rows)}] {model_id}: resume {old.get('status')}", flush=True)
            continue
        repo = (row.get("modelscope_repos") or [""])[0]
        if not repo:
            result = {"model_id": model_id, "status": "skipped_no_modelscope", "yaml": row.get("yaml")}
            results.append(result)
            print(f"[{index}/{len(rows)}] {model_id}: {result['status']}", flush=True)
            continue
        if args.dry_run:
            result = {"model_id": model_id, "modelscope_repo": repo, "status": "ready", "yaml": row.get("yaml")}
            results.append(result)
            print(f"[{index}/{len(rows)}] {model_id}: {result['status']}", flush=True)
            continue
        model_dir, error = download_model(repo, args.models_root)
        if model_dir is None:
            result = {"model_id": model_id, "modelscope_repo": repo, "status": error, "yaml": row.get("yaml")}
        else:
            result = run_one({**row, "modelscope_repo": repo}, args, model_dir, args.log_dir)
            result["model_dir"] = str(model_dir)
        results.append(result)
        print(f"[{index}/{len(rows)}] {model_id}: {result.get('status')}", flush=True)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(results, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(results, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    counts: dict[str, int] = {}
    for item in results:
        counts[item.get("status", "unknown")] = counts.get(item.get("status", "unknown"), 0) + 1
    print(json.dumps({"output": str(args.output), "models": len(results), "counts": counts}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
