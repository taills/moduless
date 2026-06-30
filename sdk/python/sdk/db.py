"""Synchronous CMDS database client for Python extensions.

All calls carry the ``x-extension-key`` gRPC metadata so Core enforces
per-extension data isolation. Extensions never connect to PostgreSQL directly.
"""

import json
from typing import Any, Dict, List, Optional

import grpc

from .proto import tunnel_pb2, tunnel_pb2_grpc


class DBClient:
    def __init__(self, core_grpc_url: str, extension_key: str):
        self._channel = grpc.insecure_channel(core_grpc_url)
        self._stub = tunnel_pb2_grpc.DatabaseServiceStub(self._channel)
        self._metadata = (("x-extension-key", extension_key),)

    def put(self, collection: str, doc_id: str, value: Dict[str, Any]) -> None:
        self._stub.Put(
            tunnel_pb2.PutRequest(
                collection=collection,
                document_id=doc_id,
                json_data=json.dumps(value).encode("utf-8"),
            ),
            metadata=self._metadata,
        )

    def get(self, collection: str, doc_id: str) -> Optional[Dict[str, Any]]:
        resp = self._stub.Get(
            tunnel_pb2.GetRequest(collection=collection, document_id=doc_id),
            metadata=self._metadata,
        )
        if not resp.found:
            return None
        return json.loads(resp.json_data.decode("utf-8"))

    def delete(self, collection: str, doc_id: str) -> None:
        self._stub.Delete(
            tunnel_pb2.DeleteRequest(collection=collection, document_id=doc_id),
            metadata=self._metadata,
        )

    def find(
        self,
        collection: str,
        filters: Optional[List[Dict[str, str]]] = None,
        limit: int = 100,
        offset: int = 0,
    ) -> List[Dict[str, Any]]:
        pb_filters = [
            tunnel_pb2.QueryFilter(
                field=f["field"], operator=f.get("operator", "="), value=str(f["value"])
            )
            for f in (filters or [])
        ]
        resp = self._stub.Find(
            tunnel_pb2.FindRequest(
                collection=collection, filters=pb_filters, limit=limit, offset=offset
            ),
            metadata=self._metadata,
        )
        return [json.loads(doc.decode("utf-8")) for doc in resp.documents]
