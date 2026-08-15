# moduless

[English](README.md) · **简体中文** · [繁體中文](README.zh-TW.md)

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

一个功能由插件构成的 Go Web 网关。插件是独立进程，由 Core 启动、监管、热加载和热更新 —— 升级期间不丢一个请求。

插件提供自己的 HTTP API、自带微前端，并且可以像 IIS Filter 那样介入网关中**任何**请求的生命周期。启用插件时它的菜单出现在控制台，禁用时立刻消失，无需刷新页面。

```
浏览器 ──HTTP──▶ Core (:80)
                    ├─ Filter 管道    pre_route → authenticate → authorize →
                    │                 pre_handler → [后端] → post_handler → log
                    ├─ /api/plugins/*  插件自己的 HTTP API
                    ├─ /plugins/*      插件的微前端
                    └─ PluginHost ──exec──▶ 插件子进程 ×N
                         │  HashiCorp go-plugin，走 unix socket
                         ▼
                       HostServices：文档存储 · 持久化队列 · 缓存 · 锁
                       配置 · 文件 · 出站 HTTP · 事件 · 日志与指标
```

Core 只监听一个端口，插件一个都不开。

## 为什么用子进程

Core 用 `exec` 启动每个插件，这是其余能力成立的前提：

- **热加载、热卸载、热更新。** 进程归 Core 所有，所以它能先启动新版本、健康检查通过后原子切换流量、再排空旧版本。实测：切换期间持续打流量，**零失败请求**。
- **崩溃隔离且可自愈。** 插件 panic 不会波及 Core。监管器按指数退避重启它，反复崩溃的则进入隔离。
- **没有网络暴露面。** 没有端口可以访问插件，也不需要注册协议来鉴权 —— Core 就是它的父进程。

## Filter

插件在 manifest 里声明关心哪些阶段和路径，Core 把它编译成匹配表。没人订阅的请求几乎不花钱：

| 场景 | 成本 |
|---|---|
| 该阶段无人订阅 | **1.9 ns**，零分配 |
| 有订阅但路径不匹配 | **8.2 ns**，零分配 |
| 真正跨进程调用一次 filter | ~37,000 ns |

Filter 默认 fail-open —— 多数 filter 是观察者，一个坏掉的观察者不该让站点不可用。任何做安全决策的 filter 必须显式声明 `fail_closed`。

## 一个插件长这样

```go
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /notes", listNotes)

	sdk.Serve(sdk.Config{
		Handler: mux,
		Filters: map[sdk.Phase]sdk.FilterFunc{
			sdk.PhasePreRoute: rateLimit,
		},
		Jobs: map[string]sdk.JobFunc{
			"nightly-summary": summarise,
		},
	})
}
```

`sdk.Serve` 接收标准 `http.Handler`，所以任何路由库和中间件都能直接用。插件能触达的一切 —— 文档存储、队列、缓存、锁、文件、出站 HTTP —— 都经过 Core，且每次调用都自动带上请求的 trace id，因此一次慢查询能归因到引发它的那个请求。

完整指南：[docs/plugin-development.md](docs/plugin-development.md)。
可运行示例：[`extension-example/notes`](extension-example/notes)。

## 运行

```bash
git clone git@github.com:taills/moduless.git
cd moduless

# 构建控制台（一次即可）
cd core/frontend && npm install && npm run build && cd ../..

# 把示例插件构建到插件目录
./scripts/build-examples.sh

# 启动。不配 DATABASE_URL 时数据、队列、文件能力会报 Unavailable，其余照常工作
PLUGIN_DIR=./plugins go run ./core
```

或者用 Docker：

```bash
docker compose up --build   # 控制台 http://localhost:8080，admin / admin123
```

## 数据

插件从不直连 PostgreSQL。它在 `manifest.yaml` 里声明 collection，Core 负责建表，访问通过文档存储进行 —— 支持排序、游标分页、聚合、批量写入、事务和乐观锁。

用游标分页而不是 OFFSET 是有意的：OFFSET 会让数据库逐行跳过并丢弃，翻得越深越慢，而且期间有行增删会导致重复或遗漏。

持久化队列基于 PostgreSQL —— 至少一次投递，带重试、退避、死信、延迟消息和去重，不需要在部署里再加一个中间件。

## 信任模型

插件由使用者审核后安装，以 Core 自身的权限运行 —— 就像 ISAPI Filter 跑在 IIS 工作进程里那样。安全性来自「只装你信任的插件」。

Core 确实会在自己这一侧强制这些：插件声明的权限集、文档/缓存/队列/文件的按插件命名空间隔离、事务归属、出站 HTTP 白名单（含拒绝解析到内网或链路本地地址的目标）、以及二进制的 SHA-256 校验。

但它不限制文件系统、CPU 或系统调用。真正不受信任的代码应当放在容器边界之后。

## 测试

```bash
go test ./... -race

# 需要数据库或对象存储的测试在未设置时自动跳过
TEST_DATABASE_URL='postgres://...' TEST_S3_ENDPOINT='http://localhost:19000' go test ./...

# 控制台
cd core/frontend && npm test

# 在 Linux 上跑一遍 —— 那才是它实际运行的地方。有两处行为与 macOS 不同且都要紧：
# 写入正在执行的二进制会得到 ETXTBSY，以及 Pdeathsig 只在 Linux 存在。
docker run --rm -v "$(pwd)":/src -w /src -e CGO_ENABLED=0 \
  golang:1.25-alpine go test ./... -count=1
```

端到端测试会 fork 真实的插件进程并通过真实 HTTP 驱动它们：持续加压下的热更新、故意制造的崩溃、一个跨越升级的请求、三个插件在同一个 Core 里相互作用，以及一次数据库重启。

[`tests/README.md`](tests/README.md) 记录了这套测试是怎么保持诚实的 —— 包括对安全边界做变异测试，以及这个代码库反复重新学到的那条规矩：断言理由，而不只是结果。

## 许可

Apache 2.0 —— 见 [`LICENSE`](LICENSE)。依赖许可审查见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
