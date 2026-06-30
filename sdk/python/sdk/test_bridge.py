"""Unit tests for the ASGI <-> tunnel bridge translation.

These do not require a running Core; they exercise the scope/receive/send
cycle against a tiny ASGI app and assert the captured HttpResponseChunk.

Run: pytest sdk/python/
"""

import asyncio
import os
import sys

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

from sdk.bridge import _dispatch_asgi  # noqa: E402
from sdk.context import get_user  # noqa: E402
from sdk.proto import tunnel_pb2  # noqa: E402


def _make_req(method="GET", path="/hello", body=b"", headers=None):
    return tunnel_pb2.HttpRequestChunk(
        stream_id="s1",
        is_first=True,
        is_last=True,
        method=method,
        path=path,
        query="",
        headers=headers or {},
        body_chunk=body,
    )


def test_dispatch_returns_response_chunk():
    async def app(scope, receive, send):
        assert scope["type"] == "http"
        await receive()
        await send(
            {
                "type": "http.response.start",
                "status": 201,
                "headers": [(b"content-type", b"application/json")],
            }
        )
        await send({"type": "http.response.body", "body": b'{"ok":true}', "more_body": False})

    async def run():
        q: asyncio.Queue = asyncio.Queue()
        await _dispatch_asgi(_make_req(), app, q)
        return await q.get()

    msg = asyncio.run(run())
    chunk = msg.http_resp_chunk
    assert chunk.status_code == 201
    assert chunk.stream_id == "s1"
    assert chunk.body_chunk == b'{"ok":true}'
    assert chunk.headers["content-type"] == "application/json"


def test_user_context_injected():
    captured = {}

    async def app(scope, receive, send):
        user = get_user()
        captured["user"] = user
        await receive()
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.body", "body": b"", "more_body": False})

    async def run():
        q: asyncio.Queue = asyncio.Queue()
        req = _make_req(headers={"X-User-Id": "777", "X-User-Roles": "admin,ops"})
        await _dispatch_asgi(req, app, q)

    asyncio.run(run())
    assert captured["user"] is not None
    assert captured["user"].user_id == "777"
    assert captured["user"].roles == ["admin", "ops"]


def test_handler_exception_returns_500():
    async def app(scope, receive, send):
        raise RuntimeError("boom")

    async def run():
        q: asyncio.Queue = asyncio.Queue()
        await _dispatch_asgi(_make_req(), app, q)
        return await q.get()

    msg = asyncio.run(run())
    assert msg.http_resp_chunk.status_code == 500
