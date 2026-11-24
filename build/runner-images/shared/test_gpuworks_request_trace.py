import asyncio
import json
import unittest

from gpuworks_request_trace import RequestTrace


class RequestTraceTests(unittest.IsolatedAsyncioTestCase):
    async def run_app(self, app, headers):
        sent = []

        async def send(message):
            sent.append(message)

        async def receive():
            return {"type": "http.disconnect"}

        await RequestTrace(app)({"type": "http", "headers": headers}, receive, send)
        return sent

    headers = [(b"x-request-id", b"req-42"), (b"x-gpuworks-debug-attempt", b"2")]

    async def test_streamed_response(self):
        chunks = [
            {"type": "http.response.start", "status": 200, "headers": []},
            {"type": "http.response.body", "body": b"private prompt", "more_body": True},
            {"type": "http.response.body", "body": b"done"},
        ]

        async def app(scope, receive, send):
            for message in chunks:
                await send(message)

        with self.assertLogs("uvicorn.error", level="INFO") as log:
            self.assertEqual(chunks, await self.run_app(app, self.headers))
        events = [json.loads(r.args[0]) for r in log.records]
        self.assertEqual([e["event"] for e in events], ["received", "response_start", "response_end"])
        self.assertEqual(events[1]["status"], 200)
        self.assertTrue(all((e["request_id"], e["attempt"]) == ("req-42", 2) for e in events))
        self.assertNotIn("private prompt", str(events))

    async def test_disconnect_and_failure_before_headers(self):
        for failure, terminal in [(RuntimeError("sensitive detail"), "error"), (asyncio.CancelledError(), "canceled")]:
            with self.subTest(terminal=terminal):
                async def app(scope, receive, send):
                    self.assertEqual(await receive(), {"type": "http.disconnect"})
                    raise failure

                with self.assertLogs("uvicorn.error", level="INFO") as log:
                    with self.assertRaises(type(failure)):
                        await self.run_app(app, self.headers)
                events = [json.loads(r.args[0]) for r in log.records]
                self.assertEqual([e["event"] for e in events], ["received", "client_disconnect", terminal])
                self.assertNotIn("sensitive detail", str(events))

    async def test_returns_without_finishing_response(self):
        async def app(scope, receive, send):
            await send({"type": "http.response.start", "status": 200})
            await send({"type": "http.response.body", "body": b"partial", "more_body": True})

        with self.assertLogs("uvicorn.error", level="INFO") as log:
            await self.run_app(app, self.headers)
        self.assertEqual([json.loads(r.args[0])["event"] for r in log.records],
                         ["received", "response_start", "incomplete_response"])

    async def test_unmarked_request_passes_through(self):
        async def app(scope, receive, send):
            await send({"type": "http.response.start", "status": 204})

        with self.assertNoLogs("uvicorn.error", level="INFO"):
            messages = await self.run_app(app, [(b"x-request-id", b"ordinary")])
        self.assertEqual(messages[0]["status"], 204)
