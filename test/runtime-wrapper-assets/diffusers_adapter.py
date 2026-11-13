"""Adapter for images that already contain Diffusers and media encoders."""

from __future__ import annotations

import base64
import io
import os
import tempfile
from typing import Any


class DiffusersAdapter:
    def __init__(self, model: str, config: dict[str, Any]):
        from diffusers import DiffusionPipeline

        load_kwargs = dict(config.get("load_kwargs", {}))
        dtype = load_kwargs.get("torch_dtype")
        if isinstance(dtype, str):
            # Catalog configuration is JSON, whereas Diffusers expects a dtype
            # object. Passing the string through can silently select float32.
            if dtype not in ("float16", "bfloat16", "float32"):
                raise ValueError("torch_dtype must be float16, bfloat16 or float32")
            import torch

            load_kwargs["torch_dtype"] = getattr(torch, dtype)
        self.pipe = DiffusionPipeline.from_pretrained(model, **load_kwargs)
        device = config.get("device")
        if device:
            self.pipe = self.pipe.to(device)
        self.call_kwargs = dict(config.get("call_kwargs", {}))

    def invoke(self, operation: str, payload: dict[str, Any], _files: dict[str, Any]) -> dict[str, Any]:
        options = dict(self.call_kwargs)
        for key in ("width", "height", "num_inference_steps", "guidance_scale"):
            if key in payload:
                options[key] = payload[key]
        output = self.pipe(prompt=payload["prompt"], **options)
        if operation == "openai.image.generate":
            image = output.images[0]
            encoded = io.BytesIO()
            image.save(encoded, format="PNG")
            return {"content_type": "application/json", "body": {"data": [{"b64_json": base64.b64encode(encoded.getvalue()).decode()}]}}
        if operation == "openai.video.generate":
            from diffusers.utils import export_to_video

            frames = output.frames[0] if output.frames and isinstance(output.frames[0], list) else output.frames
            name = ""
            try:
                with tempfile.NamedTemporaryFile(prefix="gpuworks-video-", suffix=".mp4", delete=False) as temporary:
                    name = temporary.name
                export_to_video(frames, name)
                with open(name, "rb") as video:
                    return {"content_type": "video/mp4", "body": video.read()}
            finally:
                if name:
                    try:
                        os.remove(name)
                    except FileNotFoundError:
                        pass
        raise ValueError(f"Diffusers adapter does not implement {operation}")


def create(model: str, config: dict[str, Any]) -> DiffusersAdapter:
    return DiffusersAdapter(model, config)
