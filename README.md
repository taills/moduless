# moduless

**English** · [简体中文](README.zh-CN.md) · [繁體中文](README.zh-TW.md)

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A low-friction, highly-isolated, container-friendly modular development framework.
A Go **Core Gateway** fronts all HTTP traffic and hosts micro-frontends in memory;
language-specific **extensions** (Go / Python / Java) open **no listening ports** —
they dial Core over a reverse gRPC tunnel and serve requests through their native
web framework (Gin / FastAPI / Spring Boot).

> Design spec: [`docs/superpowers/specs/2026-06-30-modular-framework-design.md`](docs/superpowers/specs/2026-06-30-modular-framework-design.md)
> Implementation plans: [`docs/superpowers/plans/`](docs/superpowers/plans/)

## Architecture at a glance

```
Browser ──HTTP──▶ Core Gateway (:80) ──reverse gRPC tunnel (:9000)──▶ Extension (Go/Python/Java)
                     │  in-memory micro-frontend cache (zip → memory)
                     │  CMDS (PostgreSQL 18 JSONB document store)
                     │  File service (RustFS / S3, clean path-param downloads)
                     │  Event bus · UI slots · audit · diagnostics
```

Core principles enforced by the implementation:

- **Zero-port extensions** — extensions only dial out to Core's gRPC port.
- **CMDS** — extensions never touch PostgreSQL; collections/indexes are declared in
  `manifest.yaml` and Core provisions `ext_<key>_<collection>` tables on registration.
- **Core-managed files** — uploads go straight to Core/RustFS returning a `file_id`;
  downloads use clean path params `/api/system/files/download/<file_id>/<token>` (no `?`).
- **In-memory FE cache** — frontend zips stream to Core and are served from memory.

## Repository layout

| Path | Description |
|------|-------------|
| `proto/` | gRPC contract (`tunnel.proto`) shared by all languages |
| `core/` | Go Core Gateway: `tunnel`, `gateway`, `db` (CMDS), `storage`, `event`, `middleware`, `main.go` |
| `sdk/go`, `sdk/python`, `sdk/java` | Extension SDKs |
| `extension-example/{go,python,java}` | Runnable example extensions (backend + frontend + manifest) |
| `db/` | sqlc config + queries; migrations live in `core/db/migrations` (embedded) |
| `scripts/` | Protobuf code-gen scripts |

## Getting the source

```bash
git clone git@github.com:taills/moduless.git
cd moduless
```

The Go module path is `github.com/taills/moduless`.

## Code generation

```bash
./scripts/gen-proto.sh            # Go stubs  -> proto/tunnel/
./scripts/gen-proto-python.sh     # Python    -> sdk/python/sdk/proto/
cd sdk/java && mvn protobuf:compile protobuf:compile-custom   # Java
sqlc generate -f db/sqlc.yaml     # CMDS / system query code
```

## Running

Core (HTTP `:80`, gRPC `:9000` by default; override with `HTTP_ADDR` / `GRPC_ADDR`):

```bash
# Tunnel + event bus only:
go run ./core
# Full stack (CMDS + files + audit) — set DATABASE_URL (+ RUSTFS_* for files):
DATABASE_URL='postgres://user:pass@localhost:5432/app?sslmode=disable' go run ./core
```

Example extensions (set `CORE_URL`, optional `MANIFEST_PATH` to auto-provision schema/slots):

```bash
MANIFEST_PATH=extension-example/go/manifest.yaml     go run ./extension-example/go/backend
python3 extension-example/python/backend/main.py
mvn spring-boot:run -pl extension-example/java/backend   # requires JDK 17
```

Then call an extension API through the gateway:

```bash
curl http://localhost/api/extensions/go_example/info
```

## Testing

```bash
go test ./core/... ./sdk/go/... ./tests/... ./manifest/...   # add -race
pytest sdk/python/
mvn test -pl sdk/java       # requires JDK 17
```

DB-backed Go tests (CMDS, file service, E2E) auto-skip unless `TEST_DATABASE_URL`
points at a PostgreSQL instance:

```bash
TEST_DATABASE_URL='postgres://postgres:pass@localhost:5432/app?sslmode=disable' go test ./...
```

## License

Licensed under the **Apache License, Version 2.0** — see [`LICENSE`](LICENSE).

All upstream dependencies compiled into the distributed artifacts use permissive
licenses (Apache-2.0 / MIT / BSD-3-Clause / ISC) compatible with Apache-2.0. The full
dependency-license review is in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
