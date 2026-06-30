"""Load an extension manifest.yaml and apply its declarations to a
RegisterRequest so Core can provision CMDS tables/indexes and register UI slots,
matching the Go SDK's behaviour.
"""

from typing import Any, Dict

import yaml

from .proto import tunnel_pb2


def load_manifest(path: str) -> Dict[str, Any]:
    with open(path, "r", encoding="utf-8") as f:
        return yaml.safe_load(f) or {}


def save_secret(path: str, secret: str) -> None:
    """Persist the Core-issued secret back into manifest.yaml.

    Only the top-level ``secret:`` line is rewritten (replaced or appended),
    leaving the rest of the file — including comments — untouched.
    """
    with open(path, "r", encoding="utf-8") as f:
        lines = f.read().splitlines()

    out = []
    replaced = False
    for line in lines:
        if line.startswith("secret:"):
            out.append(f'secret: "{secret}"')
            replaced = True
        else:
            out.append(line)
    if not replaced:
        out.append(f'secret: "{secret}"')

    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(out) + "\n")


def apply_manifest(req: "tunnel_pb2.RegisterRequest", manifest: Dict[str, Any]) -> None:
    weight = manifest.get("weight")
    if weight:
        req.weight = int(weight)

    database = manifest.get("database") or {}
    for collection in database.get("collections", []) or []:
        col = req.collections.add()
        col.name = collection["name"]
        for index in collection.get("indexes", []) or []:
            idx = col.indexes.add()
            idx.fields.extend(index.get("fields", []) or [])
            idx.unique = bool(index.get("unique", False))

    for slot in manifest.get("ui_slots", []) or []:
        s = req.slots.add()
        s.slot_name = slot.get("slot_name", "")
        s.component_entry = slot.get("component_entry", "")

    menu = manifest.get("menu") or {}
    if menu.get("icon"):
        req.menu_icon = menu["icon"]
    if menu.get("path"):
        req.menu_path = menu["path"]
    if manifest.get("display_name"):
        req.display_name = manifest["display_name"]
