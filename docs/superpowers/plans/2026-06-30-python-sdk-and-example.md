# Python SDK & Extension Example Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create the Python SDK enabling Python extensions to connect via the gRPC reverse tunnel, mapping HTTP requests to ASGI applications (like FastAPI), exposing CMDS Database client APIs, and providing a complete React micro-frontend example.

**Architecture:** The Python SDK manages a background event loop running a gRPC client channel. It translates `HttpRequestChunk` messages from Core into ASGI HTTP scopes, executes them against a FastAPI application, and captures ASGI `http.response.start` and `http.response.body` messages to stream them back as `HttpResponseChunk` messages.

**Tech Stack:** Python 3.10+, FastAPI, `grpcio`, `grpcio-tools`, Uvicorn.

## Global Constraints

- Python SDK must bridge to any standard ASGI-compatible framework.
- No listening ports on Python extensions. All traffic tunnels through gRPC.
- Structure output example under `extension-example/python/{frontend,backend}`.

---

### Task 1: Python Protobuf Generation & Environment Setup

Compile Protobuf and gRPC definitions for Python and setup the basic development environment with poetry/pip.

**Files:**
- Create: `sdk/python/requirements.txt`
- Create: `scripts/gen-proto-python.sh`

**Interfaces:**
- Produces: Python generated files:
  - `sdk/python/sdk/proto/tunnel_pb2.py`
  - `sdk/python/sdk/proto/tunnel_pb2_grpc.py`

- [ ] **Step 1: Write python dependency requirements**

Create `sdk/python/requirements.txt`:
```txt
grpcio==1.54.2
grpcio-tools==1.54.2
fastapi==0.95.2
pydantic==1.10.8
uvicorn==0.22.0
```

- [ ] **Step 2: Write python code generation script**

Create `scripts/gen-proto-python.sh`:
```bash
#!/bin/bash
mkdir -p sdk/python/sdk/proto
python3 -m grpc_tools.protoc -Iproto/ \
    --python_out=sdk/python/sdk/proto \
    --grpc_python_out=sdk/python/sdk/proto \
    proto/tunnel.proto

# Fix relative imports in generated Python files
sed -i '' 's/import tunnel_pb2/from . import tunnel_pb2/g' sdk/python/sdk/proto/tunnel_pb2_grpc.py
```

- [ ] **Step 3: Execute python code generation and verify**

Run: `pip3 install -r sdk/python/requirements.txt && chmod +x scripts/gen-proto-python.sh && ./scripts/gen-proto-python.sh`
Expected: Files created at `sdk/python/sdk/proto/tunnel_pb2.py` and `sdk/python/sdk/proto/tunnel_pb2_grpc.py`.

- [ ] **Step 4: Commit**

```bash
git add sdk/python/requirements.txt scripts/gen-proto-python.sh sdk/python/sdk/proto/
git commit -m "feat: setup Python protobuf files and environments"
```

---

### Task 2: Python ASGI HTTP Tunnel Bridge

Implement the ASGI-to-gRPC router bridge in Python, matching tunnel payloads to FastAPI routes.

**Files:**
- Create: `sdk/python/sdk/bridge.py`
- Create: `sdk/python/sdk/context.py`
- Create: `sdk/python/sdk/test_bridge.py`

**Interfaces:**
- Produces: `start_sdk(app: FastAPI, config: Config) -> None`

- [ ] **Step 1: Write `sdk/python/sdk/context.py` for user mapping**

```python
from contextvars import ContextVar
from typing import List, Optional
from pydantic import BaseModel

class UserContext(BaseModel):
    user_id: str
    roles: List[str]
    permissions: List[str]

_user_context: ContextVar[Optional[UserContext]] = ContextVar("user_context", default=None)

def get_user() -> Optional[UserContext]:
    return _user_context.get()
```

- [ ] **Step 2: Write `sdk/python/sdk/bridge.py` ASGI runner**

```python
import asyncio
from typing import Dict, Any
import grpc
from fastapi import FastAPI
from .proto import tunnel_pb2, tunnel_pb2_grpc
from .context import _user_context, UserContext

class Config:
    def __init__(self, extension_key: str, core_grpc_url: str, is_dev: bool = True):
        self.extension_key = extension_key
        self.core_grpc_url = core_grpc_url
        self.is_dev = is_dev

def start_sdk(app: FastAPI, config: Config):
    async def run():
        async with grpc.aio.insecure_channel(config.core_grpc_url) as channel:
            stub = tunnel_pb2_grpc.ExtensionTunnelStub(channel)
            while True:
                try:
                    # Establish stream
                    async def register_gen():
                        yield tunnel_pb2.TunnelMessage(
                            register_req=tunnel_pb2.RegisterRequest(
                                extension_key=config.extension_key,
                                version="1.0.0",
                                is_dev=config.is_dev
                            )
                        )
                        # Keep generator alive
                        while True:
                            await asyncio.sleep(3600)

                    stream = stub.Connect(register_gen())
                    async for msg in stream:
                        if msg.HasField("http_req_chunk"):
                            asyncio.create_task(dispatch_asgi(msg.http_req_chunk, app, stream))
                except Exception as e:
                    print(f"Connection error: {e}. Retrying in 2s...")
                    await asyncio.sleep(2)

    asyncio.run(run())

async def dispatch_asgi(req_chunk: Any, app: FastAPI, stream: Any):
    scope = {
        "type": "http",
        "asgi": {"version": "3.0", "spec_version": "2.0"},
        "http_version": "1.1",
        "method": req_chunk.method,
        "path": req_chunk.path,
        "raw_path": req_chunk.path.encode("utf-8"),
        "query_string": req_chunk.query.encode("utf-8"),
        "headers": [(k.lower().encode("utf-8"), v.encode("utf-8")) for k, v in req_chunk.headers.items()],
    }

    # Extract user contexts
    user_id = req_chunk.headers.get("X-User-Id")
    if user_id:
        roles = req_chunk.headers.get("X-User-Roles", "").split(",")
        perms = req_chunk.headers.get("X-User-Permissions", "").split(",")
        _user_context.set(UserContext(user_id=user_id, roles=roles, permissions=perms))

    response_headers = {}
    status_code = 200
    body_buffer = bytearray()

    async def receive():
        return {
            "type": "http.request",
            "body": req_chunk.body_chunk,
            "more_body": False
        }

    async def send(message):
        nonlocal status_code, response_headers
        if message["type"] == "http.response.start":
            status_code = message["status"]
            response_headers = {k.decode("utf-8"): v.decode("utf-8") for k, v in message["headers"]}
        elif message["type"] == "http.response.body":
            body_buffer.extend(message.get("body", b""))
            if not message.get("more_body", False):
                # Send complete response chunk back over grpc
                await stream.write(tunnel_pb2.TunnelMessage(
                    http_resp_chunk=tunnel_pb2.HttpResponseChunk(
                        stream_id=req_chunk.stream_id,
                        is_first=True,
                        is_last=True,
                        status_code=status_code,
                        headers=response_headers,
                        body_chunk=bytes(body_buffer)
                    )
                ))

    await app(scope, receive, send)
```

- [ ] **Step 3: Commit**

```bash
git add sdk/python/sdk/bridge.py sdk/python/sdk/context.py
git commit -m "feat: implement Python ASGI HTTP tunnel bridge"
```

---

### Task 3: Python CMDS Client & Example Project Setup

Create Python SDK database client wrappers and build the Python example extension.

**Files:**
- Create: `sdk/python/sdk/db.py`
- Create: `extension-example/python/backend/main.py`
- Create: `extension-example/python/frontend/package.json`

**Interfaces:**
- Produces: `sdk.db.put(...)` / `sdk.db.get(...)`
- Produces: Runnable Python FastAPI backend in `extension-example/python/backend/main.py`

- [ ] **Step 1: Write `sdk/python/sdk/db.py` DB client**

```python
import json
import grpc
from typing import Any, Dict
from .proto import tunnel_pb2, tunnel_pb2_grpc

class DBClient:
    def __init__(self, core_grpc_url: str):
        self.channel = grpc.insecure_channel(core_grpc_url)
        self.client = tunnel_pb2_grpc.DatabaseServiceStub(self.channel)

    def put(self, collection: str, doc_id: str, value: Dict[str, Any]):
        json_data = json.dumps(value).encode("utf-8")
        self.client.Put(tunnel_pb2.PutRequest(
            collection=collection,
            document_id=doc_id,
            json_data=json_data
        ))

    def get(self, collection: str, doc_id: str) -> Dict[str, Any]:
        resp = self.client.Get(tunnel_pb2.GetRequest(
            collection=collection,
            document_id=doc_id
        ))
        if not resp.found:
            return {}
        return json.loads(resp.json_data.decode("utf-8"))
```

- [ ] **Step 2: Write Python Example application `extension-example/python/backend/main.py`**

```python
from fastapi import FastAPI, Depends
from sdk.bridge import start_sdk, Config
from sdk.context import get_user, UserContext

app = FastAPI()

@app.get("/info")
def get_info(user: UserContext = Depends(get_user)):
    return {
        "language": "python",
        "user_id": user.user_id if user else "anonymous"
    }

if __name__ == "__main__":
    cfg = Config(
        extension_key="python-example",
        core_grpc_url="localhost:9000",
        is_dev=True
    )
    start_sdk(app, cfg)
```

- [ ] **Step 3: Commit**

```bash
git add sdk/python/sdk/db.py extension-example/python/
git commit -m "feat: implement Python CMDS client and example application templates"
```
