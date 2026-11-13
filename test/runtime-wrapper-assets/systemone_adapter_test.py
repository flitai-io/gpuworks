#!/usr/bin/env python3
"""The System One adapter against a fake inference.py with Intern-Decision's interface."""

import base64
import hashlib
import importlib.util
import io
import json
import pathlib
import sys
import tempfile
import textwrap
import unittest

HERE = pathlib.Path(__file__).parent

# The fake keeps Intern-Decision's contract: validate_request raises ValueError
# for a bad request, predict re-validates and reads image files relative to
# media_root, and a too-long input fails inside predict with "truncation is
# forbidden".
FAKE_INFERENCE = textwrap.dedent('''
    from __future__ import annotations
    from dataclasses import dataclass
    from pathlib import Path

    @dataclass(frozen=True)
    class Seen:
        state: object

    def validate_request(request):
        if not isinstance(request, dict) or "state" not in request:
            raise ValueError("Supply a JSON object containing state and questions.")
        questions = request.get("questions")
        if not isinstance(questions, dict) or not 1 <= len(questions) <= 16:
            raise ValueError("Supply 1-16 questions.")
        for image in request.get("images") or []:
            if not isinstance(image, str) or not image:
                raise ValueError("Images must be nonempty local file paths.")
        return request

    class DecisionEngine:
        def __init__(self, checkpoint=None, *, media_root=".", device="cuda", dtype="bfloat16", max_length=8192):
            self.media_root = Path(media_root)
            self.options = {"checkpoint": checkpoint, "device": device, "dtype": dtype, "max_length": max_length}
            self.calls = []
            self.fail = None

        def predict(self, request):
            validate_request(request)
            self.calls.append(Seen(request["state"]))
            if self.fail:
                raise self.fail
            if request["state"] == "too long":
                raise ValueError("Example has 9000 tokens, above 8192; truncation is forbidden")
            images = [(self.media_root / name).read_bytes() for name in request.get("images", [])]
            answers = {name: {"type": "noul", "noul": 0.25, "probabilities": {"no": 0.75, "yes": 0.25}, "confidence": 0.75}
                       for name in request["questions"]}
            return {"model": "Intern-Decision-Fake", "answers": answers, "images_seen": len(images),
                    "usage": {"input_tokens": 12, "output_tokens": len(answers)}}
''')

PNG_1x1 = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="
)
QUESTIONS = {"q": {"type": "noul", "instructions": "Is it a number?"}}


def load(name):
    spec = importlib.util.spec_from_file_location(name, HERE / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def have_pillow():
    return importlib.util.find_spec("PIL") is not None


class SystemOneAdapterTest(unittest.TestCase):
    def test_catalog_bundles_contain_tested_sources(self):
        import tarfile

        models = HERE.parents[1] / "internal/server/inferencecatalog/seed/catalog/models/internlm"
        for size in ("0.8b", "2b", "4b"):
            bundle = models / f"intern-decision-{size}/assets/gpuworks-systemone-runtime.tar.gz"
            with self.subTest(size), tarfile.open(bundle) as archive:
                self.assertEqual(sorted(archive.getnames()), ["gpuworks_serve.py", "systemone_adapter.py"])
                for name in archive.getnames():
                    self.assertEqual(archive.extractfile(name).read(), (HERE / name).read_bytes())

    def setUp(self):
        self.adapter_module = load("systemone_adapter")
        self.model = tempfile.TemporaryDirectory()
        self.addCleanup(self.model.cleanup)
        self.script = pathlib.Path(self.model.name) / "inference.py"
        self.script.write_text(FAKE_INFERENCE)
        self.digest = hashlib.sha256(self.script.read_bytes()).hexdigest()
        self.addCleanup(sys.modules.pop, "gpuworks_systemone_inference", None)

    def create(self, **config):
        adapter = self.adapter_module.create(self.model.name, {"inference_sha256": self.digest, **config})
        self.addCleanup(adapter.close)
        return adapter

    def test_refuses_inference_code_the_template_did_not_pin(self):
        with self.assertRaisesRegex(ValueError, "inference_sha256"):
            self.adapter_module.create(self.model.name, {})
        with self.assertRaisesRegex(RuntimeError, "refusing to run it"):
            self.adapter_module.create(self.model.name, {"inference_sha256": "0" * 64})

    def test_loads_engine_with_template_options_and_warms_up(self):
        adapter = self.create(dtype="float16", device="cuda:0", max_length=4096)
        self.assertEqual(adapter.engine.options, {"checkpoint": self.model.name, "device": "cuda:0", "dtype": "float16", "max_length": 4096})
        self.assertEqual(adapter.engine.media_root, adapter.media_root)
        self.assertEqual([call.state for call in adapter.engine.calls], ["1"])

    def test_forwards_only_the_wire_fields_and_echoes_the_requested_model(self):
        adapter = self.create()
        result = adapter.invoke("systemone.decide", {"model": "my-service", "state": {"a": 1}, "questions": QUESTIONS, "stream": True}, {})
        self.assertEqual(result["content_type"], "application/json")
        body = result["body"]
        self.assertEqual(body["model"], "my-service")
        self.assertEqual(body["answers"]["q"]["noul"], 0.25)
        self.assertEqual(body["usage"], {"input_tokens": 12, "output_tokens": 1})
        self.assertEqual(adapter.engine.calls[-1].state, {"a": 1})

    def test_request_faults_are_400_and_engine_faults_are_not(self):
        adapter = self.create()
        RequestError = self.adapter_module.RequestError
        with self.assertRaises(RequestError) as missing_state:
            adapter.invoke("systemone.decide", {"questions": QUESTIONS}, {})
        self.assertEqual(missing_state.exception.http_status, 400)
        with self.assertRaisesRegex(RequestError, "1-16 questions"):
            adapter.invoke("systemone.decide", {"state": "x", "questions": {}}, {})
        with self.assertRaises(RequestError) as too_long:
            adapter.invoke("systemone.decide", {"state": "too long", "questions": QUESTIONS}, {})
        self.assertEqual(too_long.exception.code, "context_length_exceeded")
        adapter.engine.fail = ValueError("Invalid probability distribution")
        with self.assertRaisesRegex(RuntimeError, "Invalid probability") as engine_fault:
            adapter.invoke("systemone.decide", {"state": "x", "questions": QUESTIONS}, {})
        self.assertNotIsInstance(engine_fault.exception, RequestError)
        with self.assertRaisesRegex(ValueError, "does not implement"):
            adapter.invoke("openai.chat", {"messages": []}, {})

    @unittest.skipUnless(have_pillow(), "Pillow is not installed")
    def test_images_are_decoded_into_the_media_root_and_removed(self):
        adapter = self.create()
        url = "data:image/png;base64," + base64.b64encode(PNG_1x1).decode()
        body = adapter.invoke("systemone.decide", {"state": "x", "questions": QUESTIONS, "images": [url, url]}, {})["body"]
        self.assertEqual(body["images_seen"], 2)
        self.assertEqual(list(adapter.media_root.iterdir()), [])

    @unittest.skipUnless(have_pillow(), "Pillow is not installed")
    def test_rejects_image_sources_other_than_checked_data_urls(self):
        adapter = self.create(max_images=2, max_image_bytes=1024)
        RequestError = self.adapter_module.RequestError
        good = "data:image/png;base64," + base64.b64encode(PNG_1x1).decode()
        cases = {
            "local path": "/etc/passwd",
            "remote URL": "https://example.com/a.png",
            "not base64": "data:image/png;base64,***",
            "not the declared format": "data:image/jpeg;base64," + base64.b64encode(PNG_1x1).decode(),
            "too large": "data:image/png;base64," + base64.b64encode(b"\0" * 2048).decode(),
        }
        for label, image in cases.items():
            with self.subTest(label), self.assertRaises(RequestError):
                adapter.invoke("systemone.decide", {"state": "x", "questions": QUESTIONS, "images": [image]}, {})
        with self.assertRaisesRegex(RequestError, "at most 2"):
            adapter.invoke("systemone.decide", {"state": "x", "questions": QUESTIONS, "images": [good] * 3}, {})
        with self.assertRaisesRegex(RequestError, "must be a list"):
            adapter.invoke("systemone.decide", {"state": "x", "questions": QUESTIONS, "images": good}, {})
        self.assertEqual(list(adapter.media_root.iterdir()), [])


class SystemOneServeTest(unittest.TestCase):
    """gpuworks_serve maps the adapter's request errors to 4xx, other faults to 500."""

    def test_adapter_request_error_status_reaches_the_client(self):
        serve = load("gpuworks_serve")
        import http.client
        import threading
        from http.server import ThreadingHTTPServer

        class Rejecting(Exception):
            http_status = 400
            code = "context_length_exceeded"

        class Adapter:
            def invoke(self, operation, payload, files):
                if payload["state"] == "bad":
                    raise Rejecting("too long")
                if payload["state"] == "boom":
                    raise RuntimeError("engine broke")
                return {"content_type": "application/json", "body": {"answers": {}, "model": payload.get("model")}}

        state = serve.RuntimeState("m", ["systemone.decide"], 2, 5)
        state.adapter, state.status = Adapter(), "ready"
        server = ThreadingHTTPServer(("127.0.0.1", 0), serve.make_handler(state, 1 << 20))
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        self.addCleanup(state.close)

        def post(body):
            connection = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=5)
            connection.request("POST", "/v1/systemone", json.dumps(body), {"Content-Type": "application/json"})
            response = connection.getresponse()
            return response.status, json.loads(response.read())

        status, body = post({"model": "svc", "state": "", "questions": QUESTIONS})
        self.assertEqual((status, body), (200, {"answers": {}, "model": "svc"}))
        status, body = post({"state": "bad", "questions": QUESTIONS})
        self.assertEqual(status, 400)
        self.assertEqual(body["error"]["code"], "context_length_exceeded")
        status, body = post({"state": "boom", "questions": QUESTIONS})
        self.assertEqual(status, 500)
        self.assertEqual(body["error"]["code"], "adapter_error")
        status, body = post({"questions": QUESTIONS})
        self.assertEqual(status, 400)
        self.assertEqual(body["error"]["code"], "missing_required_field")


if __name__ == "__main__":
    unittest.main()
