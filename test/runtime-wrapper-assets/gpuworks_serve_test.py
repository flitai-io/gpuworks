#!/usr/bin/env python3

import importlib.util
import json
import pathlib
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock
import urllib.error
import urllib.request


MODULE_PATH = pathlib.Path(__file__).with_name("gpuworks_serve.py")
SPEC = importlib.util.spec_from_file_location("gpuworks_serve", MODULE_PATH)
SERVE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader
SPEC.loader.exec_module(SERVE)


class FakeAdapter:
    def invoke(self, operation, payload, files):
        if payload.get("sleep"):
            time.sleep(float(payload["sleep"]))
        if operation == "openai.chat":
            return {"content_type": "application/json", "body": {"choices": [{"delta": {"content": "hello"}}]}}
        if operation == "openai.embeddings":
            return {"content_type": "application/json", "body": {"data": [{"embedding": [0.1, 0.2]}]}}
        if operation == "openai.rerank":
            return {"content_type": "application/json", "body": {"results": [{"index": 0, "relevance_score": 1.0}]}}
        if operation == "openai.audio.transcribe":
            return {"content_type": "application/json", "body": {"text": files["file"]["filename"]}}
        if operation == "openai.audio.speech":
            return {"content_type": "audio/wav", "body": b"RIFF-audio"}
        if operation == "openai.image.generate":
            return {"content_type": "application/json", "body": {"data": [{"b64_json": "iVBORw0KGgo="}]}}
        if operation == "openai.video.generate":
            return {"content_type": "video/mp4", "body": b"\0\0\0\x18ftypisom"}
        raise ValueError(operation)


class ServerFixture:
    def __init__(self, timeout=1.0, max_request_bytes=1 << 20, max_jobs=64, job_ttl=3600):
        self.state = SERVE.RuntimeState("fixture-model", list(SERVE.OPERATIONS), 1, timeout, max_jobs, job_ttl)
        self.state.adapter = FakeAdapter()
        self.state.status = "ready"
        self.server = SERVE.ThreadingHTTPServer(("127.0.0.1", 0), SERVE.make_handler(self.state, max_request_bytes))
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.state.close()
        self.thread.join(timeout=2)

    def request(self, method, path, body=None, content_type="application/json"):
        data = None if body is None else body if isinstance(body, bytes) else json.dumps(body).encode()
        request = urllib.request.Request(self.url + path, data=data, method=method)
        if data is not None:
            request.add_header("Content-Type", content_type)
        try:
            response = urllib.request.urlopen(request, timeout=3)
            return response.status, response.headers.get_content_type(), response.read()
        except urllib.error.HTTPError as error:
            result = error.code, error.headers.get_content_type(), error.read()
            error.close()
            return result


class CallableServerTest(unittest.TestCase):
    def setUp(self):
        self.fixture = ServerFixture()

    def tearDown(self):
        self.fixture.close()

    def test_loader_exception_keeps_terminal_health_response_available(self):
        failure = ModuleNotFoundError("No module named 'diffusers'")
        with mock.patch.object(SERVE.importlib, "import_module", side_effect=failure):
            with mock.patch.object(SERVE.traceback, "print_exc"):
                self.fixture.state.load("diffusers_adapter", {"device": "cuda"})
        for _ in range(2):
            with self.assertRaises(urllib.error.HTTPError) as caught:
                urllib.request.urlopen(self.fixture.url + "/health", timeout=3)
            with caught.exception as response:
                self.assertEqual(response.code, 503)
                self.assertEqual(response.headers["Server"].split()[0], "GPUWorksCallable/1")
                self.assertEqual(json.loads(response.read()), {
                    "status": "failed", "error": "ModuleNotFoundError: No module named 'diffusers'",
                })

    def test_health_models_and_six_synchronous_operations(self):
        status, _, body = self.fixture.request("GET", "/health")
        self.assertEqual((status, json.loads(body)["status"]), (200, "ready"))
        status, _, body = self.fixture.request("GET", "/v1/models")
        self.assertEqual((status, json.loads(body)["data"][0]["id"]), (200, "fixture-model"))

        cases = [
            ("/v1/chat/completions", {"messages": [{"role": "user", "content": "hello"}], "stream": True}, "text/event-stream", b"data: [DONE]"),
            ("/v1/embeddings", {"input": "hello"}, "application/json", b"embedding"),
            ("/v1/rerank", {"query": "hello", "documents": ["one"]}, "application/json", b"relevance_score"),
            ("/v1/audio/speech", {"input": "hello"}, "audio/wav", b"RIFF"),
            ("/v1/images/generations", {"prompt": "sunrise"}, "application/json", b"b64_json"),
        ]
        for path, payload, content_type, expected in cases:
            with self.subTest(path=path):
                status, actual_type, body = self.fixture.request("POST", path, payload)
                self.assertEqual((status, actual_type), (200, content_type))
                self.assertIn(expected, body)

        boundary = "gpuworks-boundary"
        multipart_body = (
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nfixture-model\r\n"
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"audio.wav\"\r\nContent-Type: audio/wav\r\n\r\nRIFF-audio\r\n"
            f"--{boundary}--\r\n"
        ).encode()
        status, actual_type, body = self.fixture.request("POST", "/v1/audio/transcriptions", multipart_body, f"multipart/form-data; boundary={boundary}")
        self.assertEqual((status, actual_type, json.loads(body)["text"]), (200, "application/json", "audio.wav"))

    def test_async_video_create_poll_content_and_cancel(self):
        status, _, body = self.fixture.request("POST", "/v1/videos", {"prompt": "sunrise"})
        job = json.loads(body)
        self.assertEqual((status, job["status"]), (202, "queued"))
        deadline = time.time() + 2
        while time.time() < deadline:
            status, _, body = self.fixture.request("GET", f"/v1/videos/{job['id']}")
            job = json.loads(body)
            if job["status"] == "completed":
                break
            time.sleep(0.01)
        self.assertEqual((status, job["status"]), (200, "completed"))
        status, content_type, body = self.fixture.request("GET", f"/v1/videos/{job['id']}/content")
        self.assertEqual((status, content_type, body), (200, "video/mp4", b"\0\0\0\x18ftypisom"))
        status, _, body = self.fixture.request("DELETE", f"/v1/videos/{job['id']}")
        self.assertEqual((status, json.loads(body)["status"]), (200, "completed"))

    def test_limits_errors_and_failed_health_are_explicit(self):
        status, _, body = self.fixture.request("POST", "/v1/embeddings", {})
        self.assertEqual((status, json.loads(body)["error"]["code"]), (400, "missing_required_field"))
        status, _, body = self.fixture.request("POST", "/v1/unknown", {})
        self.assertEqual((status, json.loads(body)["error"]["code"]), (404, "operation_not_declared"))
        with self.fixture.state.lock:
            self.fixture.state.status = "failed"
            self.fixture.state.error = "model load failed"
        status, _, body = self.fixture.request("GET", "/health")
        self.assertEqual((status, json.loads(body)["error"]), (503, "model load failed"))

    def test_timeout_does_not_turn_into_success(self):
        self.fixture.close()
        self.fixture = ServerFixture(timeout=0.01)
        status, _, body = self.fixture.request("POST", "/v1/embeddings", {"input": "hello", "sleep": 0.05})
        self.assertEqual((status, json.loads(body)["error"]["code"]), (504, "call_timeout"))

    def test_video_jobs_are_bounded_and_expired_terminal_jobs_are_pruned(self):
        self.fixture.close()
        self.fixture = ServerFixture(max_jobs=1, job_ttl=10)
        status, _, body = self.fixture.request("POST", "/v1/videos", {"prompt": "one"})
        first = json.loads(body)
        self.assertEqual(status, 202)
        deadline = time.time() + 2
        while time.time() < deadline:
            _, _, body = self.fixture.request("GET", f"/v1/videos/{first['id']}")
            if json.loads(body)["status"] == "completed":
                break
            time.sleep(0.01)
        status, _, body = self.fixture.request("POST", "/v1/videos", {"prompt": "two"})
        self.assertEqual((status, json.loads(body)["error"]["code"]), (429, "video_job_limit"))
        with self.fixture.state.lock:
            self.fixture.state.jobs[first["id"]]["updated_at"] = 0
        status, _, body = self.fixture.request("POST", "/v1/videos", {"prompt": "three"})
        self.assertEqual((status, json.loads(body)["status"]), (202, "queued"))

    def test_model_local_special_handler_uses_the_same_small_adapter_contract(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            (root / "special_handler.py").write_text(
                "class Handler:\n"
                "    def invoke(self, operation, payload, files):\n"
                "        return {'content_type':'application/json','body':{'operation':operation,'value':payload['input']}}\n"
                "def create(model, config):\n"
                "    return Handler()\n",
                encoding="utf-8",
            )
            state = SERVE.RuntimeState("model", ["openai.embeddings"], 1, 1)
            sys.path.insert(0, temporary)
            try:
                state.load("special_handler:create", {})
                self.assertEqual(state.status, "ready")
                result = state.invoke("openai.embeddings", {"input": "value"}, {})
                self.assertEqual(result["body"], {"operation": "openai.embeddings", "value": "value"})
            finally:
                sys.path.remove(temporary)
                sys.modules.pop("special_handler", None)
                state.close()


if __name__ == "__main__":
    unittest.main()
