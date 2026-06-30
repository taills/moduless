#!/bin/bash
set -e
OUT_DIR="sdk/python/sdk/proto"
mkdir -p "$OUT_DIR"
touch "$OUT_DIR/__init__.py"

python3 -m grpc_tools.protoc -Iproto/ \
    --python_out="$OUT_DIR" \
    --grpc_python_out="$OUT_DIR" \
    proto/tunnel.proto

# Rewrite the absolute import in the generated grpc file to a package-relative
# one so `from sdk.proto import tunnel_pb2_grpc` works. macOS BSD sed syntax.
if [[ "$OSTYPE" == "darwin"* ]]; then
    sed -i '' 's/^import tunnel_pb2 as/from . import tunnel_pb2 as/' "$OUT_DIR/tunnel_pb2_grpc.py"
else
    sed -i 's/^import tunnel_pb2 as/from . import tunnel_pb2 as/' "$OUT_DIR/tunnel_pb2_grpc.py"
fi

echo "Python protobuf stubs generated at $OUT_DIR/"
