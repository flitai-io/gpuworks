"""Adapter for System One decision models whose inference code ships with the weights.

A System One decision model (Intern-Decision and other open-weight Jev-style
models) takes a shared state and typed questions and returns a probability
distribution per question from one forward pass; it generates nothing. Their
repositories carry their own ``inference.py`` with a ``DecisionEngine`` and a
``validate_request``. This adapter loads that file, but only when its SHA-256
matches the one the inference template pins: code from a weights directory runs
in the container, so a different file (another revision, a third-party mirror,
a local directory registered under the same name) must not.

The wire format is TypeSafe's System One API: ``{model, state, questions,
images?}`` in, ``{model, answers, usage}`` out. Images arrive as base64 data
URLs; the engine reads image files, so each request's images are decoded,
checked and written under the adapter's own media root and removed afterwards.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import importlib.util
import io
import re
import shutil
import sys
import tempfile
import threading
import uuid
from pathlib import Path
from typing import Any


OPERATION = "systemone.decide"
DATA_URL = re.compile(r"^data:image/(png|jpeg|jpg|webp);base64,(.*)$", re.DOTALL)
IMAGE_FORMATS = {"png": "PNG", "jpeg": "JPEG", "jpg": "JPEG", "webp": "WEBP"}
WARMUP_REQUEST = {"state": "1", "questions": {"q": {"type": "noul", "instructions": "Is the state a number?"}}}


class RequestError(Exception):
    """A request the model cannot take; gpuworks_serve answers it with 400."""

    http_status = 400

    def __init__(self, message: str, code: str = "invalid_request"):
        super().__init__(message)
        self.code = code


class SystemOneAdapter:
    def __init__(self, model: str, config: dict[str, Any]):
        root = Path(model)
        script = root / str(config.get("inference_file", "inference.py"))
        expected = str(config.get("inference_sha256", "")).strip().lower()
        if not re.fullmatch(r"[0-9a-f]{64}", expected):
            raise ValueError("adapter config inference_sha256 must be the 64-hex SHA-256 of the model's inference code")
        source = script.read_bytes()
        actual = hashlib.sha256(source).hexdigest()
        if actual != expected:
            raise RuntimeError(f"{script.name} has SHA-256 {actual}, the template pins {expected}; refusing to run it")
        self.module = load_inference_module(script, source)
        self.max_images = int(config.get("max_images", 8))
        self.max_image_bytes = int(config.get("max_image_bytes", 16 << 20))
        self.max_image_pixels = int(config.get("max_image_pixels", 40_000_000))
        self.media_root = Path(tempfile.mkdtemp(prefix="gpuworks-systemone-"))
        self.lock = threading.Lock()
        self.engine = self.module.DecisionEngine(
            str(root),
            media_root=str(self.media_root),
            device=str(config.get("device", "cuda")),
            dtype=str(config.get("dtype", "bfloat16")),
            max_length=int(config.get("max_length", 8192)),
        )
        # The first forward pass initialises CUDA kernels; do it before /health
        # reports ready, so the first user request is not the slow one.
        self.predict(WARMUP_REQUEST)

    def invoke(self, operation: str, payload: dict[str, Any], _files: dict[str, Any]) -> dict[str, Any]:
        if operation != OPERATION:
            raise ValueError(f"System One adapter does not implement {operation}")
        if "state" not in payload:
            raise RequestError("state is required")
        request: dict[str, Any] = {"state": payload["state"], "questions": payload.get("questions")}
        workdir = self.media_root / uuid.uuid4().hex
        try:
            images = self.store_images(payload.get("images"), workdir)
            if images:
                request["images"] = images
            result = self.predict(request)
        finally:
            shutil.rmtree(workdir, ignore_errors=True)
        # The gateway forwards a callable's request unchanged, so the client's
        # model name is the service's name; the engine only knows its own.
        result["model"] = payload.get("model") or result.get("model")
        return {"content_type": "application/json", "body": result}

    def predict(self, request: dict[str, Any]) -> dict[str, Any]:
        try:
            self.module.validate_request(request)
        except ValueError as exc:
            raise RequestError(str(exc)) from exc
        with self.lock:
            try:
                return self.engine.predict(request)
            except ValueError as exc:
                # After validation, the only input fault left is a request
                # longer than the context; anything else (a marker mismatch,
                # non-finite probabilities) is the server's.
                if "truncation is forbidden" in str(exc):
                    raise RequestError(str(exc), "context_length_exceeded") from exc
                raise RuntimeError(str(exc)) from exc

    def store_images(self, images: Any, workdir: Path) -> list[str]:
        if images in (None, []):
            return []
        if not isinstance(images, list):
            raise RequestError("images must be a list of data URLs")
        if len(images) > self.max_images:
            raise RequestError(f"at most {self.max_images} images are accepted")
        from PIL import Image

        workdir.mkdir(parents=True)
        names = []
        for index, image in enumerate(images):
            match = DATA_URL.match(image) if isinstance(image, str) else None
            if not match:
                raise RequestError(f"images[{index}] must be a data:image/png, jpeg or webp base64 URL")
            kind, encoded = match.groups()
            if len(encoded) > (self.max_image_bytes * 4) // 3 + 4:
                raise RequestError(f"images[{index}] is larger than {self.max_image_bytes} bytes")
            try:
                raw = base64.b64decode(encoded, validate=True)
            except (binascii.Error, ValueError) as exc:
                raise RequestError(f"images[{index}] is not valid base64") from exc
            if len(raw) > self.max_image_bytes:
                raise RequestError(f"images[{index}] is larger than {self.max_image_bytes} bytes")
            try:
                with Image.open(io.BytesIO(raw), formats=[IMAGE_FORMATS[kind]]) as decoded:
                    width, height = decoded.size
                    decoded.verify()
            except Exception as exc:
                raise RequestError(f"images[{index}] is not a readable {kind} image") from exc
            if width * height > self.max_image_pixels:
                raise RequestError(f"images[{index}] has more than {self.max_image_pixels} pixels")
            name = f"{workdir.name}/{index}.{kind}"
            (self.media_root / name).write_bytes(raw)
            names.append(name)
        return names

    def close(self) -> None:
        shutil.rmtree(self.media_root, ignore_errors=True)


def load_inference_module(script: Path, source: bytes) -> Any:
    """Run the verified bytes, never a second read of the file."""
    name = "gpuworks_systemone_inference"
    spec = importlib.util.spec_from_loader(name, loader=None, origin=str(script))
    module = importlib.util.module_from_spec(spec)
    module.__file__ = str(script)
    # dataclasses resolve string annotations through sys.modules.
    sys.modules[name] = module
    try:
        exec(compile(source, str(script), "exec"), module.__dict__)
    except BaseException:
        sys.modules.pop(name, None)
        raise
    return module


def create(model: str, config: dict[str, Any]) -> SystemOneAdapter:
    return SystemOneAdapter(model, config)
