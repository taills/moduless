# Third-Party Notices / 第三方依赖许可证 / 第三方相依授權

`moduleless` is released under the **Apache License, Version 2.0**. This document
records the upstream-dependency license review performed for that release.

本项目以 **Apache License 2.0** 发布。本文件记录了为该发布所做的上游依赖许可证核查结论。

本專案以 **Apache License 2.0** 發佈。本文件記錄了為該發佈所做的上游相依授權核查結論。

---

## Conclusion / 结论 / 結論

**Every dependency that is actually compiled into the distributed artifacts uses a
permissive license (Apache-2.0, MIT, BSD-3-Clause, or ISC), all of which are
compatible with Apache-2.0. No copyleft license (GPL/LGPL/AGPL/MPL/EPL/CDDL) is
linked into any shipped binary.**

实际编译进分发产物的每一个依赖均采用宽松许可证（Apache-2.0 / MIT / BSD-3-Clause / ISC），
全部与 Apache-2.0 兼容；任何 copyleft 许可证（GPL/LGPL/AGPL/MPL/EPL/CDDL）都**未**被链接进
交付的二进制。

實際編譯進發佈產物的每一個相依皆採用寬鬆授權（Apache-2.0 / MIT / BSD-3-Clause / ISC），
全部與 Apache-2.0 相容；任何 copyleft 授權（GPL/LGPL/AGPL/MPL/EPL/CDDL）皆**未**被連結進交付的二進位。

### Verification method / 核查方法 / 核查方法

The review used the **actual compiled import graph** (`go list -deps ./...`), not
the full module require graph (`go list -m all`). This distinction matters: Go only
links packages that are imported, so optional drivers pulled transitively into the
module graph but never imported are **not** part of the distributed work.

核查依据的是**实际编译导入图**（`go list -deps ./...`），而非完整模块依赖图
（`go list -m all`）。这一区别很关键：Go 只链接被 import 的包，模块图中被间接拉入但
从未 import 的可选驱动**不属于**分发产物。

---

## Go — components linked into the build / 实际链接的 Go 组件

| Module | License |
|--------|---------|
| google.golang.org/grpc | Apache-2.0 |
| google.golang.org/genproto/googleapis/rpc | Apache-2.0 |
| google.golang.org/protobuf | BSD-3-Clause |
| github.com/aws/aws-sdk-go-v2 (+ config, credentials, service/s3, sts, sso, …) | Apache-2.0 |
| github.com/aws/smithy-go | Apache-2.0 |
| github.com/golang-migrate/migrate/v4 | MIT |
| github.com/lib/pq | MIT |
| github.com/google/uuid | BSD-3-Clause |
| gopkg.in/yaml.v3 | MIT (with Apache-2.0 portions) |
| golang.org/x/net, golang.org/x/sys, golang.org/x/text, golang.org/x/crypto | BSD-3-Clause |
| github.com/gin-gonic/gin (example backend only) | MIT |
| gin transitive deps (go-playground/validator, ugorji/go/codec, pelletier/go-toml, mattn/go-isatty, leodido/go-urn, goccy/go-yaml, gabriel-vasile/mimetype, gin-contrib/sse, quic-go, …) | MIT / BSD-3 / Apache-2.0 |

### Note on golang-migrate optional drivers / 关于 golang-migrate 可选驱动的说明

`golang-migrate` declares many database drivers in its module graph (MySQL, Mongo,
Spanner, ClickHouse, etc.). One of these, `github.com/go-sql-driver/mysql`, is
**MPL-2.0**. moduleless imports only the **PostgreSQL** source/driver, so the MySQL
driver and other optional drivers are **not compiled into the binary** and are not
redistributed. Packagers who choose to vendor the entire module graph (rather than
running a standard `go build`) should be aware that the vendored tree would then
include MPL-2.0 files; MPL-2.0 is file-level copyleft and remains Apache-2.0
compatible for distribution, but standard builds avoid it entirely.

`golang-migrate` 在其模块图中声明了大量数据库驱动（MySQL、Mongo、Spanner、ClickHouse 等）。
其中 `github.com/go-sql-driver/mysql` 为 **MPL-2.0**。本项目仅 import **PostgreSQL** 驱动，
故 MySQL 等可选驱动**不会被编译进二进制**，也不参与分发。若打包者选择 vendor 整个模块图
（而非标准 `go build`），需注意 vendored 目录将包含 MPL-2.0 文件；MPL-2.0 为文件级 copyleft，
分发上仍与 Apache-2.0 兼容，但标准构建完全不涉及它。

---

## Python SDK

| Package | License |
|---------|---------|
| grpcio | Apache-2.0 |
| grpcio-tools | Apache-2.0 |
| protobuf | BSD-3-Clause |
| fastapi | MIT |
| pydantic | MIT |
| uvicorn | BSD-3-Clause |

## Java SDK

| Artifact | License |
|----------|---------|
| io.grpc:grpc-* | Apache-2.0 |
| com.google.protobuf:protobuf-java | BSD-3-Clause |
| com.fasterxml.jackson.core:jackson-databind | Apache-2.0 |
| org.springframework:spring-webmvc, spring-test | Apache-2.0 |
| org.springframework.boot:spring-boot-* (example) | Apache-2.0 |
| org.apache.tomcat:annotations-api | Apache-2.0 |
| jakarta.servlet:jakarta.servlet-api | EPL-2.0 / GPL-2.0-with-Classpath-Exception |

### Note on jakarta.servlet-api / 关于 jakarta.servlet-api 的说明

`jakarta.servlet-api` is the standard Servlet specification API, dual-licensed under
EPL-2.0 and GPL-2.0-with-Classpath-Exception. It is declared with Maven **`provided`**
scope — it is a compile-time interface supplied at runtime by the servlet container
(Tomcat) and is **not bundled or redistributed** by moduleless. Using a spec API at
compile time under these terms does not impose copyleft obligations on this project.

`jakarta.servlet-api` 是标准 Servlet 规范 API，采用 EPL-2.0 与 GPL-2.0（含 Classpath 例外）
双重许可。它以 Maven **`provided`** 作用域声明——属于编译期接口，运行时由 Servlet 容器（Tomcat）
提供，moduleless **不打包、不分发**它。在此条款下于编译期使用规范 API 不会对本项目施加 copyleft 义务。

---

## Reproducing the review / 复现核查

```bash
# Modules actually linked into the artifacts (the set that matters for distribution):
go list -deps ./... | xargs go list -f '{{if .Module}}{{.Module.Path}}@{{.Module.Version}}{{end}}' | sort -u

# Inspect each module's LICENSE under $(go env GOMODCACHE).
```
