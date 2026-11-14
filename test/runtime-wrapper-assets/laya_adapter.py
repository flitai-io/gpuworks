"""Adapter for Laya System One decision checkpoints.

Laya (convaiinnovations/laya and its multilingual and typed-decisions
siblings) is a non-autoregressive decision model on a ModernBERT / mmBERT
encoder: a state plus typed questions in, one probability distribution per
question out, from a single forward pass. Its inference code is the ``laya``
package, which the System One runner image installs; the weights directory only
holds safetensors, the encoder config and the tokenizer.

The wire format is TypeSafe's System One API, the same one ``laya-serve``
exposes: ``{model, state, questions}`` in, ``{model, answers, usage}`` out. The
response is projected onto the strict contract (what ``LAYA_JEV_STRICT`` serves),
so a client sees the same fields from every System One template. The request
limits are ``laya.serve``'s numbers, but every refusal is a 400 like the other
System One adapters' (laya.serve answers some with 413 or 422).

``laya.load`` rewrites ``tokenizer/tokenizer_config.json`` in place when the
installed transformers cannot read it (the multilingual checkpoint stores
``extra_special_tokens`` as a list). The weights directory is shared and may be
read-only, so the checkpoint is loaded through a private directory that links the
weights and holds its own copy of the tokenizer.
"""

from __future__ import annotations

import logging
import os
import shutil
import tempfile
import threading
from pathlib import Path
from typing import Any


OPERATION = "systemone.decide"
WARMUP_STATE = "1"
WARMUP_QUESTIONS = {"q": {"type": "noul", "instructions": "Is the state a number?"}}

# laya.serve's limits for /v1/systemone.
MAX_QUESTIONS = 64
MAX_STATE_CHARS = 50_000
MAX_CHOICE_OPTIONS = 100
MAX_SCORE_LEVELS = 32
MAX_TOTAL_OPTIONS = 512

CHECKPOINT_FILES = ("rl_agent_config.json", "model.safetensors", "config.json")
CHECKPOINT_DIRS = ("encoder",)

log = logging.getLogger("gpuworks.laya")


class RequestError(Exception):
    """A request the model cannot take; gpuworks_serve answers with http_status."""

    def __init__(self, message: str, code: str = "invalid_request", http_status: int = 400):
        super().__init__(message)
        self.code = code
        self.http_status = http_status


class LayaAdapter:
    def __init__(self, model: str, config: dict[str, Any], loader: Any = None):
        root = Path(model)
        if not (root / "rl_agent_config.json").is_file():
            raise FileNotFoundError(f"{root} is not a Laya checkpoint: rl_agent_config.json is missing")
        self.lock = threading.Lock()
        self.workdir = Path(tempfile.mkdtemp(prefix="gpuworks-laya-"))
        try:
            checkpoint = stage_checkpoint(root, self.workdir / "checkpoint")
            if loader is None:
                # Everything the checkpoint needs is local; a Hub lookup would
                # only stall a container without egress.
                os.environ.setdefault("HF_HUB_OFFLINE", "1")
                import laya

                loader = laya.load
            # An absolute directory never reaches the Hub; the checkpoint's
            # encoder is built from its own encoder/config.json.
            device = str(config.get("device", "cuda"))
            self.agent = loader(str(checkpoint), device=device)
            # Laya falls back to the CPU with only a warning when the GPU is
            # unusable (no driver libraries, a driver older than the CUDA
            # build, a bad ordinal); a deployment that reserved a GPU must
            # fail instead of answering slowly from the CPU.
            actual = str(getattr(self.agent, "device", ""))
            if device.startswith("cuda") and not actual.startswith("cuda"):
                raise RuntimeError(f"Laya loaded on {actual or 'an unknown device'}, not {device}: the GPU is not usable in this container")
            # The first forward pass initialises CUDA kernels; do it before
            # /health reports ready, so the first user request is not the
            # slow one.
            self.agent.predict(WARMUP_STATE, WARMUP_QUESTIONS)
        except BaseException:
            self.close()
            raise

    def invoke(self, operation: str, payload: dict[str, Any], _files: dict[str, Any]) -> dict[str, Any]:
        if operation != OPERATION:
            raise ValueError(f"Laya adapter does not implement {operation}")
        state = payload.get("state")
        questions = payload.get("questions")
        check_request(state, questions)
        if payload.get("images") not in (None, []):
            raise RequestError("this model reads text only; send the state without images")
        with self.lock:
            try:
                result = self.agent.predict(state, questions)
            except ValueError as exc:
                # Laya's question validation names the question and what to fix.
                raise RequestError(str(exc)) from exc
        usage = result.get("usage") or {}
        if usage.get("truncated"):
            log.warning("state truncated: %s of %s tokens dropped",
                        usage.get("state_tokens_dropped"), usage.get("state_tokens"))
        body = project_strict(result)
        # The gateway forwards a callable's request unchanged, so the client's
        # model name is the service's name; Laya only knows its own.
        body["model"] = payload.get("model") or body.get("model")
        return {"content_type": "application/json", "body": body}

    def close(self) -> None:
        shutil.rmtree(self.workdir, ignore_errors=True)


def stage_checkpoint(root: Path, target: Path) -> Path:
    """Link the weights into target and copy the tokenizer, which Laya may rewrite."""
    target.mkdir(parents=True)
    for name in CHECKPOINT_FILES + CHECKPOINT_DIRS:
        if (root / name).exists():
            os.symlink((root / name).resolve(), target / name)
    tokenizer = root / "tokenizer"
    if not tokenizer.is_dir():
        raise FileNotFoundError(f"{root} is not a Laya checkpoint: tokenizer/ is missing")
    # copyfile, not copy2: the copy must be writable even when the weights are not.
    shutil.copytree(tokenizer, target / "tokenizer", copy_function=shutil.copyfile)
    for path in (target / "tokenizer").rglob("*"):
        path.chmod(0o700 if path.is_dir() else 0o600)
    (target / "tokenizer").chmod(0o700)
    return target


def check_request(state: Any, questions: Any) -> None:
    """laya.serve's checks, before anything is tokenised."""
    try:
        check_limits(state, questions)
    except RecursionError as exc:
        raise RequestError("the request is nested too deeply") from exc


def check_limits(state: Any, questions: Any) -> None:
    if state is None:
        raise RequestError("state is required")
    if not isinstance(questions, dict):
        raise RequestError("questions must be an object")
    if not questions:
        # Laya answers {} with empty answers; every System One template
        # refuses it, so a client sees one contract.
        raise RequestError("supply at least one question")
    if len(questions) > MAX_QUESTIONS:
        raise RequestError(f"too many questions ({len(questions)} > {MAX_QUESTIONS})")
    total = 0
    for name, question in questions.items():
        if not isinstance(question, dict):
            continue
        kind, criteria = question.get("type"), question.get("criteria")
        if kind == "choice" and isinstance(criteria, (dict, list)):
            total += len(criteria)
            if len(criteria) > MAX_CHOICE_OPTIONS:
                raise RequestError(f"too many choice options for {name!r} ({len(criteria)} > {MAX_CHOICE_OPTIONS})")
        elif kind == "score" and isinstance(criteria, list):
            total += len(criteria)
            if len(criteria) > MAX_SCORE_LEVELS:
                raise RequestError(f"too many score levels for {name!r} ({len(criteria)} > {MAX_SCORE_LEVELS})")
            if None in criteria:
                raise RequestError(f"score question {name!r} has a null level at index {criteria.index(None)}")
    if total > MAX_TOTAL_OPTIONS:
        raise RequestError(f"too many answer options across questions ({total} > {MAX_TOTAL_OPTIONS})")
    if state_length(state) > MAX_STATE_CHARS:
        raise RequestError(f"state is longer than {MAX_STATE_CHARS} characters", "context_length_exceeded")
    if has_lone_surrogate(state) or has_lone_surrogate(questions):
        raise RequestError("the request contains an unpaired UTF-16 surrogate")


def state_length(state: Any) -> int:
    if isinstance(state, str):
        return len(state)
    import json

    try:
        return len(json.dumps(state, ensure_ascii=False))
    except (TypeError, ValueError) as exc:
        raise RequestError("state must be text or JSON") from exc


def has_lone_surrogate(value: Any) -> bool:
    if isinstance(value, str):
        return any(0xD800 <= ord(ch) <= 0xDFFF for ch in value)
    if isinstance(value, dict):
        return any(has_lone_surrogate(k) or has_lone_surrogate(v) for k, v in value.items())
    if isinstance(value, list):
        return any(has_lone_surrogate(v) for v in value)
    return False


def project_strict(result: dict[str, Any]) -> dict[str, Any]:
    """The strict System One response: model, answers and the two token counts.

    Laya adds a routing report, per-answer action heads and truncation facts;
    a client validating the contract with no extra fields may reject them. No
    value is recomputed, and an answer of an unknown shape passes unchanged.
    """
    answers: dict[str, Any] = {}
    for name, answer in (result.get("answers") or {}).items():
        kind = answer.get("type") if isinstance(answer, dict) else None
        if kind == "choice":
            answers[name] = {key: answer[key] for key in ("type", "choice", "confidence", "probabilities")}
        elif kind == "score":
            answers[name] = {key: answer[key] for key in ("type", "score", "confidence", "probabilities", "legend")}
        elif kind == "noul":
            answers[name] = {"type": "noul", "noul": answer["noul"]}
        else:
            answers[name] = answer
    usage = result.get("usage") or {}
    return {
        "model": result.get("model"),
        "answers": answers,
        "usage": {"input_tokens": usage.get("input_tokens", 0), "output_tokens": usage.get("output_tokens", 0)},
    }


def create(model: str, config: dict[str, Any]) -> LayaAdapter:
    return LayaAdapter(model, config)
