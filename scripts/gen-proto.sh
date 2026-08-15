#!/bin/bash
# Generates the Go stubs for the plugin contract. Run from the repo root.
#
#   proto/plugin.proto  Host -> Plugin
#   proto/host.proto    Plugin -> Host (over the go-plugin broker)
set -e

mkdir -p proto/plugin
protoc --go_out=. --go_opt=module=github.com/taills/moduless \
       --go-grpc_out=. --go-grpc_opt=module=github.com/taills/moduless \
       proto/plugin.proto proto/host.proto
echo "Go protobuf stubs generated at proto/plugin/"
