"""ASGI <-> gRPC reverse-tunnel bridge for Python extensions.

Core delivers HTTP requests over a bidirectional gRPC stream. This module
drives a single stream using an outgoing :class:`asyncio.Queue` as the request
iterator (so we can write responses back onto the same call), translates each
``HttpRequestChunk`` into an ASGI ``scope``/``receive``/``send`` cycle against
the user's FastAPI app, and pushes the captured response back as a
``HttpResponseChunk``.
"""

import asyncio
import hashlib
import io
import os
import zipfile
from typing import Any

import grpc

from .context import UserContext, _user_context
from .proto import tunnel_pb2, tunnel_pb2_grpc

# Each FileChunk payload stays well under gRPC's 4MB default message limit.
_FRONTEND_CHUNK_SIZE = 256 * 1024


class Config:
    def __init__(
        self,
        extension_key: str,
        core_grpc_url: str,
        is_dev: bool = True,
        version: str = "1.0.0",
        dev_frontend_url: str = "",
        frontend_dir: str = "",
    ):
        self.extension_key = extension_key
        self.core_grpc_url = core_grpc_url
        self.is_dev = is_dev
        self.version = version
        self.dev_frontend_url = dev_frontend_url
        # frontend_dir, when set in production mode (is_dev=False), points at the
        # built micro-frontend directory. The SDK zips it and streams it to Core
        # during registration so Core serves the assets from its gateway.
        self.frontend_dir = frontend_dir


def _build_frontend_zip(directory: str) -> tuple[bytes, str]:
    """Zip ``directory`` in memory with slash-separated relative entry names.

    Core extracts each entry keyed by ``"/"+name``, served by the gateway at
    ``/extensions/<key>/<name>``. Returns the zip bytes and their hex SHA-256.
    """
    if not os.path.isdir(directory):
        raise NotADirectoryError(f"frontend dir not found: {directory}")

    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w", zipfile.ZIP_DEFLATED) as zf:
        for root, _dirs, files in os.walk(directory):
            for name in files:
                abs_path = os.path.join(root, name)
                arcname = os.path.relpath(abs_path, directory).replace(os.sep, "/")
                zf.write(abs_path, arcname)
    data = buffer.getvalue()
    return data, hashlib.sha256(data).hexdigest()


def start_sdk(app: Any, config: Config) -> None:
    """Blocking entrypoint: connect, register and serve forever."""
    asyncio.run(_run(app, config))


async def _run(app: Any, config: Config) -> None:
    # Bundle the micro-frontend once; it is re-streamed on every reconnect.
    frontend_zip = b""
    register_is_dev = config.is_dev
    zip_sha256 = ""
    if not config.is_dev:
        if config.frontend_dir:
            frontend_zip, zip_sha256 = _build_frontend_zip(config.frontend_dir)
        else:
            print("no frontend_dir set with is_dev=False; registering without a micro-frontend")
            register_is_dev = True

    while True:
        try:
            async with grpc.aio.insecure_channel(config.core_grpc_url) as channel:
                stub = tunnel_pb2_grpc.ExtensionTunnelStub(channel)
                out_queue: "asyncio.Queue[tunnel_pb2.TunnelMessage]" = asyncio.Queue()

                # First outgoing message registers the extension.
                await out_queue.put(
                    tunnel_pb2.TunnelMessage(
                        register_req=tunnel_pb2.RegisterRequest(
                            extension_key=config.extension_key,
                            version=config.version,
                            is_dev=register_is_dev,
                            dev_frontend_url=config.dev_frontend_url,
                            zip_file_size=len(frontend_zip),
                            zip_sha256=zip_sha256,
                        )
                    )
                )

                # Stream the bundled frontend, then signal completion so Core
                # extracts it and replies with the registration result.
                if frontend_zip:
                    for index in range(0, len(frontend_zip), _FRONTEND_CHUNK_SIZE):
                        await out_queue.put(
                            tunnel_pb2.TunnelMessage(
                                file_chunk=tunnel_pb2.FileChunk(
                                    content=frontend_zip[index : index + _FRONTEND_CHUNK_SIZE],
                                    chunk_index=index // _FRONTEND_CHUNK_SIZE,
                                )
                            )
                        )
                    await out_queue.put(
                        tunnel_pb2.TunnelMessage(
                            register_complete=tunnel_pb2.RegisterComplete()
                        )
                    )

                async def request_iter():
                    while True:
                        msg = await out_queue.get()
                        yield msg

                call = stub.Connect(request_iter())
                async for msg in call:
                    if msg.HasField("register_resp"):
                        if not msg.register_resp.success:
                            print(f"registration rejected: {msg.register_resp.error_message}")
                            break
                        print("registration success")
                    elif msg.HasField("http_req_chunk"):
                        asyncio.create_task(
                            _dispatch_asgi(msg.http_req_chunk, app, out_queue)
                        )
                    elif msg.HasField("pong"):
                        pass
        except Exception as e:  # noqa: BLE001 - reconnect on any failure
            print(f"connection error: {e}. retrying in 2s...")
            await asyncio.sleep(2)


async def _dispatch_asgi(req_chunk: Any, app: Any, out_queue: "asyncio.Queue") -> None:
    headers = dict(req_chunk.headers)
    scope = {
        "type": "http",
        "asgi": {"version": "3.0", "spec_version": "2.1"},
        "http_version": "1.1",
        "method": req_chunk.method,
        "scheme": "http",
        "path": req_chunk.path,
        "raw_path": req_chunk.path.encode("utf-8"),
        "query_string": req_chunk.query.encode("utf-8"),
        "root_path": "",
        "headers": [
            (k.lower().encode("utf-8"), v.encode("utf-8")) for k, v in headers.items()
        ],
        "client": ("127.0.0.1", 0),
        "server": ("core", 80),
    }

    # Inject authenticated user from Core-provided headers.
    user_id = headers.get("X-User-Id")
    if user_id:
        roles = [r for r in headers.get("X-User-Roles", "").split(",") if r]
        perms = [p for p in headers.get("X-User-Permissions", "").split(",") if p]
        _user_context.set(UserContext(user_id=user_id, roles=roles, permissions=perms))

    state = {"status": 200, "headers": {}, "body": bytearray()}
    body_sent = False

    async def receive():
        nonlocal body_sent
        if not body_sent:
            body_sent = True
            return {
                "type": "http.request",
                "body": bytes(req_chunk.body_chunk),
                "more_body": False,
            }
        # No further body; keep the app from hanging if it polls again.
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message):
        if message["type"] == "http.response.start":
            state["status"] = message["status"]
            state["headers"] = {
                k.decode("utf-8"): v.decode("utf-8") for k, v in message["headers"]
            }
        elif message["type"] == "http.response.body":
            state["body"].extend(message.get("body", b""))
            if not message.get("more_body", False):
                await out_queue.put(
                    tunnel_pb2.TunnelMessage(
                        http_resp_chunk=tunnel_pb2.HttpResponseChunk(
                            stream_id=req_chunk.stream_id,
                            is_first=True,
                            is_last=True,
                            status_code=state["status"],
                            headers=state["headers"],
                            body_chunk=bytes(state["body"]),
                        )
                    )
                )

    try:
        await app(scope, receive, send)
    except Exception as e:  # noqa: BLE001 - return 500 instead of dropping the stream
        await out_queue.put(
            tunnel_pb2.TunnelMessage(
                http_resp_chunk=tunnel_pb2.HttpResponseChunk(
                    stream_id=req_chunk.stream_id,
                    is_first=True,
                    is_last=True,
                    status_code=500,
                    headers={"Content-Type": "text/plain"},
                    body_chunk=f"internal error: {e}".encode("utf-8"),
                )
            )
        )
