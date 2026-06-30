# moduleless

[English](README.md) · **简体中文** · [繁體中文](README.zh-TW.md)

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

一套**低上手门槛、高代码隔离、部署容器化、调试简易**的模块化开发框架。
Go 编写的**核心网关（Core Gateway）**统一承载所有 HTTP 流量并在内存中托管微前端；
各语言的**扩展模块**（Go / Python / Java）**不开放任何监听端口**——它们通过反向 gRPC 隧道
主动连接 Core，并用各自原生的 Web 框架（Gin / FastAPI / Spring Boot）处理请求。

> 设计规范：[`docs/superpowers/specs/2026-06-30-modular-framework-design.md`](docs/superpowers/specs/2026-06-30-modular-framework-design.md)
> 实现计划：[`docs/superpowers/plans/`](docs/superpowers/plans/)

## 架构速览

```
浏览器 ──HTTP──▶ Core 网关 (:80) ──反向 gRPC 隧道 (:9000)──▶ 扩展 (Go/Python/Java)
                    │  内存微前端缓存（zip → 内存）
                    │  CMDS（PostgreSQL 18 JSONB 文档存储）
                    │  文件服务（RustFS / S3，纯路径参数下载）
                    │  事件总线 · UI 插槽 · 审计 · 诊断
```

实现所强制遵守的核心准则：

- **零端口扩展**——扩展只对外拨号连接 Core 的 gRPC 端口。
- **CMDS**——扩展从不直连 PostgreSQL；在 `manifest.yaml` 中声明集合/索引，Core 在注册时
  自动创建 `ext_<key>_<collection>` 数据表。
- **Core 托管文件**——上传直达 Core/RustFS 并返回 `file_id`；下载使用纯路径参数
  `/api/system/files/download/<file_id>/<token>`（不含 `?` 查询串）。
- **内存前端缓存**——前端 zip 流式推送到 Core，从内存直接提供服务。

## 仓库结构

| 路径 | 说明 |
|------|------|
| `proto/` | 各语言共享的 gRPC 契约（`tunnel.proto`） |
| `core/` | Go 核心网关：`tunnel`、`gateway`、`db`（CMDS）、`storage`、`event`、`middleware`、`main.go` |
| `sdk/go`、`sdk/python`、`sdk/java` | 各语言扩展 SDK |
| `extension-example/{go,python,java}` | 可运行的示例扩展（后端 + 前端 + manifest） |
| `db/` | sqlc 配置与查询；迁移文件位于 `core/db/migrations`（已内嵌） |
| `scripts/` | Protobuf 代码生成脚本 |

## 获取源码

```bash
git clone git@github.com:taills/moduleless.git
cd moduleless
```

Go 模块路径为 `github.com/taills/moduleless`。

## 代码生成

```bash
./scripts/gen-proto.sh            # Go 桩代码  -> proto/tunnel/
./scripts/gen-proto-python.sh     # Python     -> sdk/python/sdk/proto/
cd sdk/java && mvn protobuf:compile protobuf:compile-custom   # Java
sqlc generate -f db/sqlc.yaml     # CMDS / 系统查询代码
```

## 运行

Core（默认 HTTP `:80`、gRPC `:9000`；可用 `HTTP_ADDR` / `GRPC_ADDR` 覆盖）：

```bash
# 仅隧道 + 事件总线：
go run ./core
# 全功能（CMDS + 文件 + 审计）——设置 DATABASE_URL（文件服务再加 RUSTFS_*）：
DATABASE_URL='postgres://user:pass@localhost:5432/app?sslmode=disable' go run ./core
```

示例扩展（设置 `CORE_URL`，可选 `MANIFEST_PATH` 以自动 provision 表结构/插槽）：

```bash
MANIFEST_PATH=extension-example/go/manifest.yaml     go run ./extension-example/go/backend
python3 extension-example/python/backend/main.py
mvn spring-boot:run -pl extension-example/java/backend   # 需要 JDK 17
```

随后通过网关调用扩展 API：

```bash
curl http://localhost/api/extensions/go_example/info
```

## 测试

```bash
go test ./core/... ./sdk/go/... ./tests/... ./manifest/...   # 可加 -race
pytest sdk/python/
mvn test -pl sdk/java       # 需要 JDK 17
```

依赖数据库的 Go 测试（CMDS、文件服务、端到端）会在未设置 `TEST_DATABASE_URL` 时自动跳过：

```bash
TEST_DATABASE_URL='postgres://postgres:pass@localhost:5432/app?sslmode=disable' go test ./...
```

## 许可证

采用 **Apache License 2.0** 发布——详见 [`LICENSE`](LICENSE)。

编译进分发产物的全部上游依赖均使用与 Apache-2.0 兼容的宽松许可证
（Apache-2.0 / MIT / BSD-3-Clause / ISC）。完整的依赖许可证核查见
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
