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
