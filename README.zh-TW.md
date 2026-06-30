# moduless

[English](README.md) · [简体中文](README.zh-CN.md) · **繁體中文**

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

一套**低上手門檻、高程式碼隔離、部署容器化、除錯簡易**的模組化開發框架。
以 Go 撰寫的**核心閘道（Core Gateway）**統一承載所有 HTTP 流量並於記憶體中託管微前端；
各語言的**擴充模組**（Go / Python / Java）**不開放任何監聽埠**——它們透過反向 gRPC 隧道
主動連線 Core，並以各自原生的 Web 框架（Gin / FastAPI / Spring Boot）處理請求。

> 設計規範：[`docs/superpowers/specs/2026-06-30-modular-framework-design.md`](docs/superpowers/specs/2026-06-30-modular-framework-design.md)
> 實作計畫：[`docs/superpowers/plans/`](docs/superpowers/plans/)

## 架構速覽

```
瀏覽器 ──HTTP──▶ Core 閘道 (:80) ──反向 gRPC 隧道 (:9000)──▶ 擴充 (Go/Python/Java)
                    │  記憶體微前端快取（zip → 記憶體）
                    │  CMDS（PostgreSQL 18 JSONB 文件儲存）
                    │  檔案服務（RustFS / S3，純路徑參數下載）
                    │  事件匯流排 · UI 插槽 · 稽核 · 診斷
```

實作所強制遵守的核心準則：

- **零埠擴充**——擴充只對外撥號連線 Core 的 gRPC 埠。
- **CMDS**——擴充從不直連 PostgreSQL；於 `manifest.yaml` 中宣告集合/索引，Core 於註冊時
  自動建立 `ext_<key>_<collection>` 資料表。
- **Core 託管檔案**——上傳直達 Core/RustFS 並回傳 `file_id`；下載使用純路徑參數
  `/api/system/files/download/<file_id>/<token>`（不含 `?` 查詢字串）。
- **記憶體前端快取**——前端 zip 串流推送至 Core，自記憶體直接提供服務。

## 儲存庫結構

| 路徑 | 說明 |
|------|------|
| `proto/` | 各語言共用的 gRPC 契約（`tunnel.proto`） |
| `core/` | Go 核心閘道：`tunnel`、`gateway`、`db`（CMDS）、`storage`、`event`、`middleware`、`main.go` |
| `sdk/go`、`sdk/python`、`sdk/java` | 各語言擴充 SDK |
| `extension-example/{go,python,java}` | 可執行的範例擴充（後端 + 前端 + manifest） |
| `db/` | sqlc 設定與查詢；遷移檔位於 `core/db/migrations`（已內嵌） |
| `scripts/` | Protobuf 程式碼產生指令稿 |

## 取得原始碼

```bash
git clone git@github.com:taills/moduless.git
cd moduless
```

Go 模組路徑為 `github.com/taills/moduless`。

## 程式碼產生

```bash
./scripts/gen-proto.sh            # Go 樁程式碼 -> proto/tunnel/
./scripts/gen-proto-python.sh     # Python      -> sdk/python/sdk/proto/
cd sdk/java && mvn protobuf:compile protobuf:compile-custom   # Java
sqlc generate -f db/sqlc.yaml     # CMDS / 系統查詢程式碼
```

## 執行

Core（預設 HTTP `:80`、gRPC `:9000`；可用 `HTTP_ADDR` / `GRPC_ADDR` 覆寫）：

```bash
# 僅隧道 + 事件匯流排：
go run ./core
# 完整功能（CMDS + 檔案 + 稽核）——設定 DATABASE_URL（檔案服務再加 RUSTFS_*）：
DATABASE_URL='postgres://user:pass@localhost:5432/app?sslmode=disable' go run ./core
```

範例擴充（設定 `CORE_URL`，可選 `MANIFEST_PATH` 以自動 provision 資料表/插槽）：

```bash
MANIFEST_PATH=extension-example/go/manifest.yaml     go run ./extension-example/go/backend
python3 extension-example/python/backend/main.py
mvn spring-boot:run -pl extension-example/java/backend   # 需要 JDK 17
```

接著透過閘道呼叫擴充 API：

```bash
curl http://localhost/api/extensions/go_example/info
```

## 測試

```bash
go test ./core/... ./sdk/go/... ./tests/... ./manifest/...   # 可加 -race
pytest sdk/python/
mvn test -pl sdk/java       # 需要 JDK 17
```

相依資料庫的 Go 測試（CMDS、檔案服務、端對端）會在未設定 `TEST_DATABASE_URL` 時自動略過：

```bash
TEST_DATABASE_URL='postgres://postgres:pass@localhost:5432/app?sslmode=disable' go test ./...
```

## 授權

以 **Apache License 2.0** 發佈——詳見 [`LICENSE`](LICENSE)。

編譯進發佈產物的全部上游相依皆使用與 Apache-2.0 相容的寬鬆授權
（Apache-2.0 / MIT / BSD-3-Clause / ISC）。完整的相依授權核查見
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
