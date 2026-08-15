#!/bin/bash
# Generates the Go stubs for every .proto in the repo. Run from the repo root.
#
#   proto/plugin.proto + proto/host.proto -> proto/plugin/   (go-plugin contract)
#   proto/tunnel.proto                    -> proto/tunnel/   (legacy reverse tunnel,
#                                                             removed in Phase 5)
set -e

mkdir -p proto/plugin
protoc --go_out=. --go_opt=module=github.com/taills/moduless \
       --go-grpc_out=. --go-grpc_opt=module=github.com/taills/moduless \
       proto/plugin.proto proto/host.proto
echo "Go protobuf stubs generated at proto/plugin/"

if [ -f proto/tunnel.proto ]; then
  mkdir -p proto/tunnel
  protoc --go_out=. --go_opt=module=github.com/taills/moduless \
         --go-grpc_out=. --go-grpc_opt=module=github.com/taills/moduless \
         proto/tunnel.proto
  echo "Go protobuf stubs generated at proto/tunnel/ (legacy)"
fi
