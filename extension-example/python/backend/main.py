"""Python extension example backend (FastAPI + framework SDK).

Dev:  python3 extension-example/python/backend/main.py
Prod: set FRONTEND_DIR to the built dist so the SDK ships it to Core.
No port is opened — the SDK dials Core's gRPC tunnel (default localhost:9000).
"""

import os
import sys
import time

# Make the SDK importable when run directly from the repo.
sys.path.insert(
    0,
    os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../sdk/python")),
)

from fastapi import FastAPI, HTTPException  # noqa: E402
from pydantic import BaseModel  # noqa: E402

from sdk import Config, get_user, start_sdk  # noqa: E402
from sdk.db import DBClient  # noqa: E402

EXTENSION_KEY = "python_example"
CORE_GRPC_URL = os.getenv("CORE_URL", "localhost:9000")
COLLECTION = "items"

app = FastAPI()
db = DBClient(CORE_GRPC_URL, EXTENSION_KEY)


class ItemIn(BaseModel):
    name: str
    code: str
    status: str = "active"


@app.get("/info")
def get_info():
    user = get_user()
    return {
        "language": "python",
        "user_id": user.user_id if user else "anonymous",
        "roles": user.roles if user else [],
    }


@app.get("/items")
def list_items(status: str = "", limit: int = 100, offset: int = 0):
    filters = [{"field": "status", "operator": "=", "value": status}] if status else None
    items = db.find(COLLECTION, filters=filters, limit=limit, offset=offset)
    return {"items": items, "count": len(items)}


@app.post("/items", status_code=201)
def create_item(payload: ItemIn):
    if not payload.name or not payload.code:
        raise HTTPException(status_code=400, detail="name and code are required")
    item = {
        "id": str(time.time_ns()),
        "name": payload.name,
        "code": payload.code,
        "status": payload.status or "active",
    }
    db.put(COLLECTION, item["id"], item)
    return item


@app.get("/items/{item_id}")
def get_item(item_id: str):
    item = db.get(COLLECTION, item_id)
    if item is None:
        raise HTTPException(status_code=404, detail="not found")
    return item


@app.put("/items/{item_id}")
def update_item(item_id: str, payload: ItemIn):
    if not payload.name or not payload.code:
        raise HTTPException(status_code=400, detail="name and code are required")
    item = {
        "id": item_id,
        "name": payload.name,
        "code": payload.code,
        "status": payload.status or "active",
    }
    db.put(COLLECTION, item_id, item)
    return item


@app.delete("/items/{item_id}")
def delete_item(item_id: str):
    db.delete(COLLECTION, item_id)
    return {"ok": True}


if __name__ == "__main__":
    frontend_dir = os.getenv("FRONTEND_DIR", "")
    manifest_path = os.getenv(
        "MANIFEST_PATH",
        os.path.abspath(os.path.join(os.path.dirname(__file__), "../manifest.yaml")),
    )
    start_sdk(
        app,
        Config(
            extension_key=EXTENSION_KEY,
            core_grpc_url=CORE_GRPC_URL,
            is_dev=frontend_dir == "",
            dev_frontend_url=os.getenv("DEV_FE_URL", "http://localhost:7101"),
            frontend_dir=frontend_dir,
            manifest_path=manifest_path,
        ),
    )
