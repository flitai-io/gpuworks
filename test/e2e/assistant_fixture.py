"""Auditable OpenAI-compatible assistant endpoint for browser acceptance.

E2E standard: test/e2e/browser-e2e-testing-standard.md
The fixture replaces only the external LLM. GPUWorks' server, AI service
configuration, read-only tools, browser and SQLite stay real.

The first completion requests ``cluster_overview``; the second, which carries
the tool result, streams a final answer. A user message containing ``slow``
keeps the connection open so the browser Stop path can be exercised.
"""
from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


TOOL_CALL_ID = "call_fixture_1"
DENIED_CALL_ID = "call_fixture_denied"
DENIED_PATH = "/api/v1/ssh-key"
# A caller that tries to make call_api write: the tool has no method field, so
# this must reach the server as an ordinary read of the path and change nothing.
WRITE_CALL_ID = "call_fixture_write"
# HOSTILE_ANSWER is upstream model output the console must present as text: markup, a script body,
# a javascript: link and a markdown image that would fetch on render. None of it may execute or be
# interpreted - the answer is untrusted input from outside the product.
HOSTILE_ANSWER = (
    "Cluster looks healthy. <img src=x onerror=\"alert(1)\"> "
    "<script>alert('xss')</script> "
    "[click me](javascript:alert(2)) "
    "<b>bold</b> done."
)
WRITE_PATH = "/api/v1/nodes-labels-probe"


def cluster_answer(content: str) -> str:
    """Derive the final answer from the actual cluster_overview tool result."""
    try:
        data = json.loads(content)
        total = data["summary"]["nodes"]["total"]
        name = data["nodes_needing_attention"]["items"][0]["name"]
    except (ValueError, KeyError, TypeError, IndexError):
        return "The cluster overview could not be read: " + content[:200]
    if (type(total) is not int or total < 0 or not isinstance(name, str)
            or "unavailable" in content.lower()):
        return "The cluster overview could not be read: " + content[:200]
    return f"The cluster overview reports {total} node(s); attention includes {name}."


def denied_answer(content: str) -> str:
    """Report the tool error instead of treating an error result as success."""
    if content.startswith("error:"):
        return "The denied route was rejected: " + content
    return "The denied route was unexpectedly readable: " + content[:200]


def tool_structure_error(messages: list[dict]) -> str:
    """The relation a tool loop must preserve between call and result.

    A final answer may only be produced from a tool result that directly
    follows its assistant tool_calls message: one call, matching call ID and
    name, and object arguments. A conversation with only [user, tool] must not
    be accepted as a successful tool round.
    """
    indexes = [index for index, message in enumerate(messages)
               if message.get("role") == "tool"]
    if len(indexes) != 1:
        return "exactly one tool result is required"
    index = indexes[0]
    if index != len(messages) - 1:
        return "the tool result must end this fixture's single tool round"
    if index == 0 or messages[index - 1].get("role") != "assistant":
        return "the tool result must follow the assistant tool call"
    calls = messages[index - 1].get("tool_calls") or []
    all_calls = [message for message in messages if message.get("tool_calls")]
    if not isinstance(calls, list) or len(calls) != 1 or len(all_calls) != 1:
        return "the preceding assistant message must carry exactly one tool call"
    call = calls[0]
    tool = messages[index]
    if (not isinstance(call, dict) or not isinstance(call.get("id"), str)
            or not isinstance(call.get("function"), dict)
            or not isinstance(tool.get("content"), str)):
        return "tool calls and results have invalid field types"
    if call.get("id") != tool.get("tool_call_id"):
        return "tool_call_id does not match the preceding call"
    function = call.get("function") or {}
    if tool.get("name") != function.get("name"):
        return "the tool name does not match the preceding call"
    try:
        arguments = json.loads(function.get("arguments"))
    except (json.JSONDecodeError, TypeError):
        return "the tool arguments are not valid JSON"
    if not isinstance(arguments, dict):
        return "the tool arguments must be an object"
    expected = {
        TOOL_CALL_ID: ("cluster_overview", {}),
        DENIED_CALL_ID: ("call_api", {"path": DENIED_PATH}),
    }.get(call.get("id"))
    if (call.get("type") != "function" or expected is None
            or (function.get("name"), arguments) != expected):
        return "the tool call differs from the fixture's issued function and arguments"
    return ""


class AssistantEndpoint:
    def __init__(self, model: str = "fixture-assistant", api_key: str = "fixture-key",
                 slow_seconds: float = 2.5):
        self.model = model
        self.api_key = api_key
        self.slow_seconds = slow_seconds
        self.requests: list[dict] = []
        self.lock = threading.Lock()
        self.server: ThreadingHTTPServer | None = None
        self.thread: threading.Thread | None = None
        self.aborted = 0
        self.slow_started = threading.Event()
        self.slow_release = threading.Event()
        self.aborted_event = threading.Event()

    @property
    def base(self) -> str:
        if self.server is None:
            raise RuntimeError("assistant fixture is not started")
        host, port = self.server.server_address
        return f"http://{host}:{port}"

    def start(self) -> "AssistantEndpoint":
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def setup(self):
                super().setup()
                self.connection.settimeout(5)

            def log_message(self, *_args):
                pass

            def _json(self, status: int, body: dict):
                raw = json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(raw)

            def _send(self, event: dict) -> bool:
                try:
                    self.wfile.write(f"data: {json.dumps(event)}\n\n".encode())
                    self.wfile.flush()
                    return True
                except (BrokenPipeError, ConnectionResetError):
                    return False

            def do_POST(self):
                if self.path not in {"/chat/completions", "/v1/chat/completions"}:
                    self._json(404, {"error": {"message": "not found"}})
                    return
                length = int(self.headers.get("Content-Length", "0"))
                try:
                    body = json.loads(self.rfile.read(length))
                except json.JSONDecodeError:
                    self._json(400, {"error": {"message": "invalid JSON"}})
                    return
                if (not isinstance(body, dict)
                        or not isinstance(body.get("messages"), list)
                        or not body["messages"]
                        or any(not isinstance(message, dict) for message in body["messages"])):
                    self._json(400, {"error": {"message": "messages must be a non-empty object list"}})
                    return
                if self.headers.get("Authorization") != f"Bearer {fixture.api_key}":
                    self._json(401, {"error": {"message": "invalid API key"}})
                    return
                messages = body.get("messages") or []
                tools = body.get("tools") or []
                errors = []
                if body.get("model") != fixture.model:
                    errors.append("model")
                if body.get("stream") is not True:
                    errors.append("stream")
                if not tools:
                    errors.append("tools")
                tool_messages = [m for m in messages if m.get("role") == "tool"]
                if tool_messages or any(message.get("tool_calls") for message in messages):
                    structure = tool_structure_error(messages)
                    if structure:
                        errors.append("tool_calls: " + structure)
                user_text = " ".join(str(m.get("content", "")) for m in messages
                                     if m.get("role") == "user")
                if "hostile" in user_text:
                    kind = "hostile"
                elif "slow" in user_text:
                    kind = "slow"
                elif "please write" in user_text:
                    kind = "write_final" if tool_messages else "write"
                elif "denied" in user_text:
                    kind = "denied_final" if tool_messages else "denied"
                elif tool_messages:
                    kind = "final"
                else:
                    kind = "tool"
                with fixture.lock:
                    fixture.requests.append({"kind": kind, "body": body,
                                             "tool_messages": tool_messages,
                                             "errors": list(errors),
                                             "authorization": self.headers.get("Authorization")})
                if errors:
                    self._json(400, {"error": {"message": "invalid request: " + ", ".join(errors)}})
                    return

                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.send_header("Connection", "close")
                self.end_headers()
                if kind == "slow":
                    # The test waits for slow_started, clicks Stop, then
                    # releases the barrier; a failed write proves the upstream
                    # request was cancelled.
                    fixture.slow_started.set()
                    fixture.slow_release.wait(timeout=fixture.slow_seconds + 10)
                    if not self._send({"choices": [{"delta": {"content": "late "}}]}):
                        with fixture.lock:
                            fixture.aborted += 1
                        fixture.aborted_event.set()
                        return
                    if not self._send({"choices": [{"delta": {}, "finish_reason": "stop"}]}):
                        with fixture.lock:
                            fixture.aborted += 1
                        fixture.aborted_event.set()
                        return
                elif kind in {"tool", "denied", "write"}:
                    if kind == "tool":
                        call_id, call_name, arguments = TOOL_CALL_ID, "cluster_overview", "{}"
                    elif kind == "write":
                        call_id, call_name = WRITE_CALL_ID, "call_api"
                        arguments = json.dumps({"path": WRITE_PATH, "method": "DELETE",
                                                "body": {"labels": {"pwned": "yes"}}})
                    else:
                        call_id, call_name = DENIED_CALL_ID, "call_api"
                        arguments = json.dumps({"path": DENIED_PATH})
                    self._send({"choices": [{"delta": {"tool_calls": [{
                        "index": 0, "id": call_id, "type": "function",
                        "function": {"name": call_name, "arguments": arguments}}]}}]})
                    self._send({"choices": [{"delta": {}, "finish_reason": "tool_calls"}]})
                elif kind == "hostile":
                    answer = HOSTILE_ANSWER
                    self._send({"choices": [{"delta": {"content": answer}}]})
                    self._send({"choices": [{"delta": {}, "finish_reason": "stop"}]})
                else:
                    content = tool_messages[0].get("content", "") if tool_messages else ""
                    call_id = tool_messages[0].get("tool_call_id", "") if tool_messages else ""
                    if kind == "write_final":
                        answer = ("write tool result: " + content
                                  if call_id == WRITE_CALL_ID else "write tool id mismatch")
                    elif kind == "denied_final":
                        answer = (denied_answer(content) if call_id == DENIED_CALL_ID
                                  else "The denied route could not be verified: "
                                       "mismatched tool_call_id.")
                    elif call_id != TOOL_CALL_ID:
                        answer = ("The cluster overview could not be read: "
                                  "mismatched tool_call_id.")
                    else:
                        answer = cluster_answer(content)
                    self._send({"choices": [{"delta": {"content": answer}}]})
                    self._send({"choices": [{"delta": {}, "finish_reason": "stop"}]})
                if not self._send({"choices": [], "usage": {"prompt_tokens": 12,
                                                             "completion_tokens": 4,
                                                             "total_tokens": 16}}):
                    if kind == "slow":
                        with fixture.lock:
                            fixture.aborted += 1
                        fixture.aborted_event.set()
                    return
                try:
                    self.wfile.write(b"data: [DONE]\n\n")
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    # The server closed the upstream stream after the browser
                    # stopped the question; that is the cancellation evidence.
                    if kind == "slow":
                        with fixture.lock:
                            fixture.aborted += 1
                        fixture.aborted_event.set()
                    return

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        # server_close joins all owned handlers after stop releases the barrier.
        self.server.daemon_threads = False
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return self

    def wait_slow_started(self, timeout: float = 10) -> bool:
        return self.slow_started.wait(timeout)

    def release_slow(self) -> None:
        self.slow_release.set()

    def wait_aborted(self, timeout: float = 10) -> bool:
        return self.aborted_event.wait(timeout)

    def stop(self) -> None:
        self.slow_release.set()
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
            self.server = None
        if self.thread is not None:
            self.thread.join(timeout=5)
            self.thread = None

    def snapshot(self) -> dict:
        with self.lock:
            return {"base": self.base if self.server else "",
                    "requests": list(self.requests), "aborted": self.aborted}

    def __enter__(self) -> "AssistantEndpoint":
        return self.start()

    def __exit__(self, *_exc) -> None:
        self.stop()
