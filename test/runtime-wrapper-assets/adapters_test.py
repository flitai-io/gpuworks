#!/usr/bin/env python3

import importlib.util
import pathlib
import sys
import tarfile
import types
import unittest
from unittest import mock


HERE = pathlib.Path(__file__).parent


def load(name):
    spec = importlib.util.spec_from_file_location(name, HERE / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    assert spec.loader
    spec.loader.exec_module(module)
    return module


class RuntimeAdaptersTest(unittest.TestCase):
    def test_ernie_catalog_bundle_contains_tested_sources(self):
        root = HERE.parents[1]
        bundle = root / "internal/server/inferencecatalog/seed/catalog/models/paddlepaddle/ernie-image/assets/gpuworks-diffusers-runtime.tar.gz"
        with tarfile.open(bundle) as archive:
            for name in ("diffusers_adapter.py", "gpuworks_serve.py"):
                self.assertEqual(archive.extractfile(name).read(), (HERE / name).read_bytes())

    def test_diffusers_resolves_json_dtype_before_loading(self):
        module = load("diffusers_adapter")
        observed = {}
        torch = types.ModuleType("torch")
        torch.bfloat16 = object()
        torch.float32 = object()
        diffusers = types.ModuleType("diffusers")

        class Pipeline:
            @staticmethod
            def from_pretrained(model, *, torch_dtype):
                # Diffusers 0.38 falls back to float32 for a JSON string.
                observed["dtype"] = torch.float32 if isinstance(torch_dtype, str) else torch_dtype
                observed["model"] = model
                return Pipeline()

            def to(self, device):
                observed["device"] = device
                return self

        diffusers.DiffusionPipeline = Pipeline
        config = {"device": "cuda", "load_kwargs": {"torch_dtype": "bfloat16"}}
        with mock.patch.dict(sys.modules, {"torch": torch, "diffusers": diffusers}):
            module.DiffusersAdapter("ernie-model", config)
            self.assertIs(observed.pop("dtype"), torch.bfloat16)
            with self.assertRaisesRegex(ValueError, "torch_dtype"):
                module.DiffusersAdapter("ernie-model", {"load_kwargs": {"torch_dtype": "unknown"}})
        self.assertEqual(observed, {"model": "ernie-model", "device": "cuda"})
        self.assertEqual(config["load_kwargs"]["torch_dtype"], "bfloat16")

    def test_transformers_normalizes_chat_vectors_and_rerank(self):
        module = load("transformers_adapter")

        class Pipe:
            def __call__(self, value, **_kwargs):
                if isinstance(value, list) and value and isinstance(value[0], dict) and "text_pair" in value[0]:
                    return [{"score": 0.75} for _ in value]
                if isinstance(value, list) and value and isinstance(value[0], dict):
                    return [{"generated_text": value + [{"role": "assistant", "content": "hello"}]}]
                return [[0.1, 0.2]]

        fake = types.ModuleType("transformers")
        fake.pipeline = lambda *_args, **_kwargs: Pipe()
        with mock.patch.dict(sys.modules, {"transformers": fake}):
            adapter = module.TransformersAdapter("model", {})
            chat = adapter.invoke("openai.chat", {"messages": [{"role": "user", "content": "hi"}]}, {})
            vector = adapter.invoke("openai.embeddings", {"input": "hi"}, {})
            rerank = adapter.invoke("openai.rerank", {"query": "q", "documents": ["d"]}, {})
        self.assertEqual(chat["body"]["choices"][0]["message"]["content"], "hello")
        self.assertEqual(vector["body"]["data"][0]["embedding"], [0.1, 0.2])
        self.assertEqual(rerank["body"]["results"][0]["relevance_score"], 0.75)

    def test_funasr_upload_is_removed_after_inference(self):
        module = load("funasr_adapter")
        observed = {}

        class AutoModel:
            def __init__(self, **_kwargs):
                pass

            def generate(self, input, **_kwargs):
                observed["path"] = input
                observed["exists_during_call"] = pathlib.Path(input).exists()
                return [{"text": "transcript"}]

        fake = types.ModuleType("funasr")
        fake.AutoModel = AutoModel
        with mock.patch.dict(sys.modules, {"funasr": fake}):
            adapter = module.FunASRAdapter("model", {})
            result = adapter.invoke("openai.audio.transcribe", {}, {"file": {"filename": "a.wav", "content": b"RIFF"}})
        self.assertEqual(result["body"]["text"], "transcript")
        self.assertTrue(observed["exists_during_call"])
        self.assertFalse(pathlib.Path(observed["path"]).exists())

    def test_diffusers_image_and_video_are_normalized_and_temp_video_removed(self):
        module = load("diffusers_adapter")
        observed = {}

        class Image:
            def save(self, target, format):
                self.format = format
                target.write(b"PNG")

        class Pipe:
            def to(self, _device):
                return self

            def __call__(self, **_kwargs):
                return types.SimpleNamespace(images=[Image()], frames=[["frame"]])

        class DiffusionPipeline:
            @staticmethod
            def from_pretrained(*_args, **_kwargs):
                return Pipe()

        fake = types.ModuleType("diffusers")
        fake.DiffusionPipeline = DiffusionPipeline
        utils = types.ModuleType("diffusers.utils")

        def export_to_video(_frames, name):
            observed["path"] = name
            pathlib.Path(name).write_bytes(b"MP4")

        utils.export_to_video = export_to_video
        with mock.patch.dict(sys.modules, {"diffusers": fake, "diffusers.utils": utils}):
            adapter = module.DiffusersAdapter("model", {})
            image = adapter.invoke("openai.image.generate", {"prompt": "x"}, {})
            video = adapter.invoke("openai.video.generate", {"prompt": "x"}, {})
        self.assertEqual(image["body"]["data"][0]["b64_json"], "UE5H")
        self.assertEqual(video["body"], b"MP4")
        self.assertFalse(pathlib.Path(observed["path"]).exists())

    def test_vision_adapter_accepts_only_inline_images_and_preserves_prompt(self):
        module = load("vision_remote_adapter")

        class LoadedImage:
            def convert(self, mode):
                self.mode = mode
                return self

        loaded = LoadedImage()
        image_api = types.SimpleNamespace(open=lambda stream: loaded if stream.read() == b"PNG" else None)
        pil = types.ModuleType("PIL")
        pil.Image = image_api
        messages = [{"role": "user", "content": [
            {"type": "text", "text": "Read this"},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64,UE5H"}},
        ]}]
        with mock.patch.dict(sys.modules, {"PIL": pil}):
            prompt, image = module.request_input(messages)
            self.assertEqual("Read this", prompt)
            self.assertIs(loaded, image)
            self.assertEqual("RGB", image.mode)
            with self.assertRaisesRegex(ValueError, "inline base64"):
                module.request_input([{"role": "user", "content": [{"type": "image_url", "image_url": {"url": "https://example.invalid/a.png"}}]}])


if __name__ == "__main__":
    unittest.main()
