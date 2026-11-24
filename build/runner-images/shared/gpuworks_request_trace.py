"""Optional vLLM ASGI middleware for requests selected by GPUWorks debug capture."""

import asyncio
import json
import logging

logger = logging.getLogger("uvicorn.error")


class RequestTrace:
    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            return await self.app(scope, receive, send)
        headers = dict(scope.get("headers", ()))
        attempt = headers.get(b"x-gpuworks-debug-attempt", b"")
        request_id = headers.get(b"x-request-id", b"")
        if not (0 < len(attempt) <= 8 and attempt.isdigit() and 0 < len(request_id) <= 255):
            return await self.app(scope, receive, send)

        def record(event, **details):
            logger.info("gpuworks request %s", json.dumps({
                "request_id": request_id.decode("latin-1"), "attempt": int(attempt),
                "event": event, **details,
            }))

        async def observe_receive():
            message = await receive()
            if message["type"] == "http.disconnect":
                record("client_disconnect")
            return message

        complete = False

        async def observe_send(message):
            nonlocal complete
            await send(message)
            if message["type"] == "http.response.start":
                record("response_start", status=message["status"])
            elif message["type"] == "http.response.body" and not message.get("more_body", False):
                complete = True
                record("response_end")

        record("received")
        try:
            await self.app(scope, observe_receive, observe_send)
        except asyncio.CancelledError:
            record("canceled")
            raise
        except Exception as error:
            record("error", error_type=type(error).__name__)
            raise
        else:
            if not complete:
                record("incomplete_response")
