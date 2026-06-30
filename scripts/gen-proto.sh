#!/bin/bash
set -e
mkdir -p proto/tunnel
protoc --go_out=. --go_opt=module=github.com/ty-lab/go-web-module \
       --go-grpc_out=. --go-grpc_opt=module=github.com/ty-lab/go-web-module \
       proto/tunnel.proto
echo "Go protobuf stubs generated at proto/tunnel/"
