#!/usr/bin/env python3
"""Small stdlib HTTP service for catalog-owned one-shot Python callables."""

from __future__ import annotations

import argparse
import importlib
import json
import signal
import threading
import time
import traceback
import uuid
from concurrent.futures import ThreadPoolExecutor, TimeoutError
from email.parser import BytesParser
from email.policy import default as email_policy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any


OPERATIONS = {
    "openai.chat": "/v1/chat/completions",
    "openai.embeddings": "/v1/embeddings",
    "openai.rerank": "/v1/rerank",
    "openai.audio.transcribe": "/v1/audio/transcriptions",
    "openai.audio.speech": "/v1/audio/speech",
    "openai.image.generate": "/v1/images/generations",
    "openai.video.generate": "/v1/videos",
    "systemone.decide": "/v1/systemone",
}


class RuntimeState:
    def __init__(self, model: str, operations: list[str], concurrency: int, timeout: float, max_jobs: int = 64, job_ttl: float = 3600):
        self.model = model
        self.operations = operations
        self.timeout = timeout
        self.status = "starting"
        self.error = ""
        self.adapter: Any = None
        self.lock = threading.Lock()
        self.slots = threading.BoundedSemaphore(concurrency)
        self.executor = ThreadPoolExecutor(max_workers=concurrency, thread_name_prefix="gpuworks-callable")
        self.jobs: dict[str, dict[str, Any]] = {}
        self.max_jobs = max_jobs
        self.job_ttl = job_ttl

    def load(self, adapter_ref: str, config: dict[str, Any]) -> None:
        try:
            module_name, separator, factory_name = adapter_ref.partition(":")
            if not separator:
                factory_name = "create"
            factory = getattr(importlib.import_module(module_name), factory_name)
            adapter = factory(self.model, config)
            with self.lock:
                self.adapter = adapter
                self.status = "ready"
        except Exception as exc:  # preserve the image-owned failure for /health
            with self.lock:
                self.status = "failed"
                self.error = f"{type(exc).__name__}: {exc}"
            traceback.print_exc()

    def invoke(self, operation: str, payload: dict[str, Any], files: dict[str, dict[str, Any]]) -> dict[str, Any]:
        with self.lock:
            if self.status != "ready" or self.adapter is None:
                raise ServiceError(503, "service_not_ready", self.error or self.status)
        if not self.slots.acquire(blocking=False):
            raise ServiceError(429, "concurrency_limit", "all callable slots are busy")

        def call() -> dict[str, Any]:
            try:
                result = self.adapter.invoke(operation, payload, files)
                if not isinstance(result, dict) or "body" not in result:
                    raise TypeError("adapter must return a mapping containing body")
                return result
            finally:
                self.slots.release()

        future = self.executor.submit(call)
        try:
            return future.result(timeout=self.timeout)
        except TimeoutError as exc:
            raise ServiceError(504, "call_timeout", f"call exceeded {self.timeout:g} seconds") from exc

    def create_video(self, payload: dict[str, Any], files: dict[str, dict[str, Any]]) -> dict[str, Any]:
        job_id = f"video-{uuid.uuid4().hex}"
        now = time.time()
        job = {"id": job_id, "status": "queued", "created_at": int(now), "updated_at": int(now)}
        with self.lock:
            self.prune_jobs_locked(now)
            if len(self.jobs) >= self.max_jobs:
                raise ServiceError(429, "video_job_limit", "too many retained video jobs")
            self.jobs[job_id] = job
        created = dict(job)

        def run() -> None:
            with self.lock:
                if job["status"] == "cancelled":
                    return
                job["status"] = "running"
                job["updated_at"] = int(time.time())
            try:
                result = self.invoke("openai.video.generate", payload, files)
                body = encode_result_body(result)
                with self.lock:
                    if job["status"] != "cancelled":
                        job.update(status="completed", content=body, content_type=result.get("content_type", "video/mp4"), updated_at=int(time.time()))
            except Exception as exc:
                with self.lock:
                    if job["status"] != "cancelled":
                        job.update(status="failed", error=str(exc), updated_at=int(time.time()))

        threading.Thread(target=run, name=job_id, daemon=True).start()
        return created

    def prune_jobs_locked(self, now: float | None = None) -> None:
        now = time.time() if now is None else now
        expired = [job_id for job_id, job in self.jobs.items()
                   if job.get("status") in {"completed", "failed", "cancelled"}
                   and now - float(job.get("updated_at", job.get("created_at", now))) >= self.job_ttl]
        for job_id in expired:
            del self.jobs[job_id]

    def close(self) -> None:
        self.executor.shutdown(wait=False, cancel_futures=True)
        with self.lock:
            adapter = self.adapter
            self.jobs.clear()
        closer = getattr(adapter, "close", None)
        if callable(closer):
            closer()


class ServiceError(Exception):
    def __init__(self, status: int, code: str, message: str):
        super().__init__(message)
        self.status = status
        self.code = code


def make_handler(state: RuntimeState, max_request_bytes: int):
    class Handler(BaseHTTPRequestHandler):
        server_version = "GPUWorksCallable/1"

        def do_GET(self) -> None:
            if self.path == "/health":
                with state.lock:
                    status, error = state.status, state.error
                self.send_json(200 if status == "ready" else 503, {"status": status, "error": error or None})
                return
            if self.path == "/v1/models":
                self.send_json(200, {"object": "list", "data": [{"id": state.model, "object": "model"}]})
                return
            job_id, content = video_route(self.path)
            if job_id:
                with state.lock:
                    state.prune_jobs_locked()
                    job = state.jobs.get(job_id)
                    snapshot = public_job(job) if job else None
                    media = job.get("content") if job and content else None
                    media_type = job.get("content_type", "video/mp4") if job else "video/mp4"
                if not job:
                    self.send_error_json(ServiceError(404, "not_found", "video job was not found"))
                elif content and job["status"] != "completed":
                    self.send_error_json(ServiceError(409, "content_not_ready", "video content is not ready"))
                elif content:
                    self.send_bytes(200, media_type, media)
                else:
                    self.send_json(200, snapshot)
                return
            self.send_error_json(ServiceError(404, "not_found", "endpoint was not found"))

        def do_DELETE(self) -> None:
            job_id, content = video_route(self.path)
            if not job_id or content:
                self.send_error_json(ServiceError(404, "not_found", "endpoint was not found"))
                return
            with state.lock:
                job = state.jobs.get(job_id)
                if job and job["status"] not in {"completed", "failed"}:
                    job["status"] = "cancelled"
                    job["updated_at"] = int(time.time())
                snapshot = public_job(job) if job else None
            if snapshot is None:
                self.send_error_json(ServiceError(404, "not_found", "video job was not found"))
            else:
                self.send_json(200, snapshot)

        def do_POST(self) -> None:
            operation = next((name for name, endpoint in OPERATIONS.items() if endpoint == self.path), "")
            if not operation or operation not in state.operations:
                self.send_error_json(ServiceError(404, "operation_not_declared", "operation is not declared by this service"))
                return
            try:
                payload, files = self.read_request(max_request_bytes)
                validate_request(operation, payload, files)
                if operation == "openai.video.generate":
                    self.send_json(202, state.create_video(payload, files))
                    return
                result = state.invoke(operation, payload, files)
                if operation == "openai.chat" and payload.get("stream"):
                    self.send_sse(result)
                else:
                    self.send_result(result)
            except ServiceError as exc:
                self.send_error_json(exc)
            except Exception as exc:
                status = getattr(exc, "http_status", None)
                if isinstance(status, int) and 400 <= status < 500:
                    # An adapter rejects a request the model cannot take by
                    # raising an exception that carries its 4xx status (it
                    # cannot import this module's ServiceError: this file runs
                    # as __main__).
                    self.send_error_json(ServiceError(status, str(getattr(exc, "code", "invalid_request")), str(exc)))
                    return
                traceback.print_exc()
                self.send_error_json(ServiceError(500, "adapter_error", f"{type(exc).__name__}: {exc}"))

        def read_request(self, limit: int) -> tuple[dict[str, Any], dict[str, dict[str, Any]]]:
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError as exc:
                raise ServiceError(400, "invalid_content_length", "invalid Content-Length") from exc
            if length <= 0 or length > limit:
                raise ServiceError(413 if length > limit else 400, "request_size", f"request must contain 1..{limit} bytes")
            raw = self.rfile.read(length)
            content_type = self.headers.get("Content-Type", "")
            if content_type.lower().startswith("application/json"):
                try:
                    value = json.loads(raw)
                except json.JSONDecodeError as exc:
                    raise ServiceError(400, "invalid_json", str(exc)) from exc
                if not isinstance(value, dict):
                    raise ServiceError(400, "invalid_json", "JSON body must be an object")
                return value, {}
            if content_type.lower().startswith("multipart/form-data"):
                return parse_multipart(content_type, raw)
            raise ServiceError(415, "unsupported_media_type", "application/json or multipart/form-data is required")

        def send_result(self, result: dict[str, Any]) -> None:
            body = result.get("body")
            content_type = result.get("content_type", "application/json")
            if content_type == "application/json":
                self.send_json(200, body)
            else:
                self.send_bytes(200, content_type, encode_result_body(result))

        def send_sse(self, result: dict[str, Any]) -> None:
            body = result.get("body")
            if isinstance(body, str):
                body = {"choices": [{"delta": {"content": body}}]}
            encoded = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode()
            data = b"data: " + encoded + b"\n\ndata: [DONE]\n\n"
            self.send_bytes(200, "text/event-stream", data, {"Cache-Control": "no-cache"})

        def send_json(self, status: int, value: Any) -> None:
            self.send_bytes(status, "application/json", json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode())

        def send_error_json(self, error: ServiceError) -> None:
            self.send_json(error.status, {"error": {"message": str(error), "type": "gpuworks_callable", "code": error.code, "param": None}})

        def send_bytes(self, status: int, content_type: str, body: bytes, headers: dict[str, str] | None = None) -> None:
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            for key, value in (headers or {}).items():
                self.send_header(key, value)
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, message: str, *args: Any) -> None:
            print(f"gpuworks-callable: {self.address_string()} {message % args}", flush=True)

    return Handler


def validate_request(operation: str, payload: dict[str, Any], files: dict[str, dict[str, Any]]) -> None:
    required = {
        "openai.chat": ("messages",), "openai.embeddings": ("input",), "openai.rerank": ("query", "documents"),
        "openai.audio.speech": ("input",), "openai.image.generate": ("prompt",), "openai.video.generate": ("prompt",),
    }
    missing = [name for name in required.get(operation, ()) if payload.get(name) in (None, "", [])]
    if operation == "openai.audio.transcribe" and "file" not in files:
        missing.append("file")
    if operation == "systemone.decide":
        # Any JSON value is a state, including "" and 0; only its absence is missing.
        missing = [name for name in ("state", "questions") if name not in payload]
    if missing:
        raise ServiceError(400, "missing_required_field", "required field(s): " + ", ".join(missing))


def parse_multipart(content_type: str, raw: bytes) -> tuple[dict[str, Any], dict[str, dict[str, Any]]]:
    message = BytesParser(policy=email_policy).parsebytes(
        f"Content-Type: {content_type}\r\nMIME-Version: 1.0\r\n\r\n".encode() + raw
    )
    if not message.is_multipart():
        raise ServiceError(400, "invalid_multipart", "multipart boundary or body is invalid")
    fields: dict[str, Any] = {}
    files: dict[str, dict[str, Any]] = {}
    for part in message.iter_parts():
        name = part.get_param("name", header="content-disposition")
        if not name:
            continue
        value = part.get_payload(decode=True) or b""
        filename = part.get_filename()
        if filename:
            files[name] = {"filename": filename, "content_type": part.get_content_type(), "content": value}
        else:
            fields[name] = value.decode("utf-8")
    return fields, files


def video_route(path: str) -> tuple[str, bool]:
    clean = path.split("?", 1)[0]
    prefix = "/v1/videos/"
    if not clean.startswith(prefix):
        return "", False
    parts = clean[len(prefix):].split("/")
    if len(parts) == 1 and parts[0]:
        return parts[0], False
    if len(parts) == 2 and parts[0] and parts[1] == "content":
        return parts[0], True
    return "", False


def public_job(job: dict[str, Any] | None) -> dict[str, Any] | None:
    if job is None:
        return None
    return {key: value for key, value in job.items() if key not in {"content", "content_type"}}


def encode_result_body(result: dict[str, Any]) -> bytes:
    body = result.get("body", b"")
    if isinstance(body, bytes):
        return body
    if isinstance(body, str):
        return body.encode()
    return json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--adapter", required=True, help="module[:factory], imported from the catalog asset mount")
    parser.add_argument("--model", required=True)
    parser.add_argument("--operation", action="append", required=True, choices=sorted(OPERATIONS))
    parser.add_argument("--adapter-config", default="{}")
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument("--max-request-bytes", type=int, default=64 << 20)
    parser.add_argument("--max-concurrency", type=int, default=1)
    parser.add_argument("--request-timeout", type=float, default=300)
    parser.add_argument("--max-video-jobs", type=int, default=64)
    parser.add_argument("--video-job-ttl", type=float, default=3600)
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    if args.max_request_bytes <= 0 or args.max_concurrency <= 0 or args.request_timeout <= 0 or args.max_video_jobs <= 0 or args.video_job_ttl <= 0:
        raise SystemExit("request and concurrency limits must be positive")
    config = json.loads(args.adapter_config)
    if not isinstance(config, dict):
        raise SystemExit("--adapter-config must be a JSON object")
    state = RuntimeState(args.model, list(dict.fromkeys(args.operation)), args.max_concurrency, args.request_timeout, args.max_video_jobs, args.video_job_ttl)
    server = ThreadingHTTPServer((args.host, args.port), make_handler(state, args.max_request_bytes))
    threading.Thread(target=state.load, args=(args.adapter, config), name="gpuworks-model-loader", daemon=True).start()
    signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=server.shutdown, daemon=True).start())
    try:
        server.serve_forever(poll_interval=0.2)
    finally:
        server.server_close()
        state.close()


if __name__ == "__main__":
    main()
