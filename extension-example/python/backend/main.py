"""Python extension example backend (FastAPI + framework SDK).

Run with: python3 extension-example/python/backend/main.py
No port is opened — the SDK dials Core's gRPC tunnel (default localhost:9000).
"""

import os
import sys

# Make the SDK importable when run directly from the repo.
sys.path.insert(
    0,
    os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../sdk/python")),
)

from fastapi import FastAPI  # noqa: E402

from sdk import Config, get_user, start_sdk  # noqa: E402
from sdk.db import DBClient  # noqa: E402

EXTENSION_KEY = "python_example"
CORE_GRPC_URL = os.getenv("CORE_URL", "localhost:9000")

app = FastAPI()
db = DBClient(CORE_GRPC_URL, EXTENSION_KEY)


@app.get("/info")
def get_info():
    user = get_user()
    return {
        "language": "python",
        "user_id": user.user_id if user else "anonymous",
        "roles": user.roles if user else [],
    }


@app.post("/items/{item_id}")
def put_item(item_id: str, payload: dict):
    db.put("items", item_id, payload)
    return {"ok": True, "id": item_id}


@app.get("/items/{item_id}")
def get_item(item_id: str):
    item = db.get("items", item_id)
    return item or {}


if __name__ == "__main__":
    start_sdk(
        app,
        Config(
            extension_key=EXTENSION_KEY,
            core_grpc_url=CORE_GRPC_URL,
            is_dev=True,
        ),
    )
