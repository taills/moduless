# 插件开发指南

Moduless 插件是一个普通的 Go 程序。它不监听端口、不连接数据库、不需要知道 Core 在哪 —— Core 把它作为子进程启动，通过一条私有连接双向通信。

## 最小插件

```go
package main

import (
	"fmt"
	"net/http"

	sdk "github.com/taills/moduless/sdk/plugin"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", sdk.User(r.Context()).Username)
	})
	sdk.Serve(sdk.Config{Handler: mux})
}
```

`sdk.Serve` 接收标准 `http.Handler`，所以 chi、gin、echo 或任何中间件都能直接用 —— SDK 把 Core 的请求还原成真正的 `*http.Request`，而不是让你面向一套自定义接口编程。

配一个 `manifest.yaml`：

```yaml
key: hello
display_name: 你好
version: 1.0.0
runtime:
  entrypoint: bin/plugin
```

打包成目录：

```
hello/
├── manifest.yaml
├── bin/plugin          # CGO_ENABLED=0 编译的静态二进制
└── frontend/           # 可选：微前端 dist
```

放进 Core 的 `PLUGIN_DIR`，在控制台「插件管理」里点启用。

---

## 三条必须知道的规则

**一、绝不向 stdout 写任何东西。**

Core 从插件 stdout 的第一行读取启动握手。一个 `fmt.Println` 就会让插件启动失败，且错误信息不会指向真正的原因。用 `sdk.Log` 或标准库 `log`（默认走 stderr，Core 会捕获）。

**二、必须 `CGO_ENABLED=0`。**

运行时镜像基于 alpine（musl）。动态链接的二进制在容器里会直接 `exec format error`。

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/plugin .
```

**三、部署新版本要用 `mv`，不能用 `cp`。**

热更新时旧版本仍在处理请求，直到切换完成。覆盖一个正在执行的二进制会破坏那个进程的内存映像。必须用 rename 或先 unlink 再写（新 inode），让旧进程继续用旧 inode。

---

## manifest.yaml 参考

```yaml
key: notes                    # 唯一标识，也是路由前缀和目录名
display_name: 笔记
version: 1.0.0

runtime:
  entrypoint: bin/plugin      # 包内相对路径
  replicas: 1                 # >1 时按平滑加权轮询分流

# Core 在自己这一侧强制这份清单；同时它也是审核者批准插件时看的东西
permissions:
  - db                        # 文档存储
  - db:tx                     # 事务（比单条语句更重的授权，单独声明）
  - queue                     # 持久化队列
  - cache                     # 缓存
  - lock                      # 分布式锁
  - events                    # 事件广播
  - cron                      # 定时任务
  - files:read                # 生成下载链接
  - files:write               # 写入/删除文件
  - http:egress               # 出站 HTTP
  - filter:authenticate       # 允许 filter 改写调用者身份

database:
  collections:                # 插件启动前由 Core 建好
    - name: notes
      indexes:
        - fields: [author]
        - fields: [created]
          unique: false

menus:                        # 启用时出现在控制台，禁用时立刻消失
  - path: /notes
    title: 笔记
    icon: file-text
    order: 20
    roles: [admin]            # 非空时只有该角色可见
    children: []

filters:
  - name: rate-limit
    phase: pre_route
    order: 10                 # 同阶段内跨插件排序，小的先跑
    match:
      paths: ["/api/**"]      # glob：精确路径、/前缀/**、/a/*/c
      methods: ["POST", "PUT"]
    timeout_ms: 50
    fail_closed: false        # 见下方「失败策略」
    needs_request_body: false # 见下方「Body」
    max_body_bytes: 65536

jobs:
  - name: nightly-summary
    cron: "17 3 * * *"        # Core 调度，多副本只跑一个，禁用即停

egress_allow:                 # 出站 HTTP 白名单
  - api.example.com
  - "*.cdn.example.net"
```

---

## Host 能力

所有能力在 `sdk.Serve` 启动后即可用，包括插件自己的初始化阶段。每次调用都由 Core 按 manifest 校验权限。

### 文档存储

```go
// 写入
version, err := sdk.DB.Put(ctx, "notes", id, note)

// 乐观锁：版本不匹配返回 FailedPrecondition，重读后重试
version, err := sdk.DB.PutIfVersion(ctx, "notes", id, note, expectedVersion)

// 读取
found, version, err := sdk.DB.Get(ctx, "notes", id, &note)

// 查询：排序 + 游标分页 + 聚合
var notes []Note
next, err := sdk.DB.Where("notes").
    Eq("author", "alice").
    Gt("created", "2026-01-01").
    SortDesc("created").
    Limit(50).
    All(ctx, &notes)

total, err := sdk.DB.Where("notes").Eq("status", "open").Count(ctx)
sums, err := sdk.DB.Where("orders").Sum(ctx, "total", "region")

// 事务（需要 db:tx）
err := sdk.DB.Tx(ctx, 30*time.Second, func(tx *sdk.TxClient) error {
    if _, err := tx.Put(ctx, "orders", orderID, order); err != nil {
        return err
    }
    return tx.Delete(ctx, "cart", cartID)
})
```

**分页用游标不用 offset**。offset 会让数据库逐行跳过并丢弃，翻得越深越慢，而且期间有行插入或删除会导致重复或遗漏。游标基于排序键加主键做行值比较，翻到第一百页和第一页一样快。代价是所有排序字段必须同方向 —— 方向混用会被拒绝，而不是悄悄返回错误的分页。

**事务有超时**。它占着一个数据库连接，所以插件崩溃时 Core 会到期回滚，不会永久占用。把事务里的活儿写短。

### 持久化队列

```go
// 发布
id, deduped, err := sdk.Queue.Publish(ctx, "emails", payload,
    sdk.WithDelay(5*time.Minute),
    sdk.WithDedupKey("order-"+orderID),
    sdk.WithMaxAttempts(3),
)

// 消费：返回 nil 即确认，返回 error 即重试
err := sdk.Queue.Consume(ctx, "emails", func(ctx context.Context, m *sdk.QueueMessage) error {
    var job EmailJob
    if err := m.Decode(&job); err != nil {
        return err  // 重试到上限后进死信
    }
    return send(job)
})
```

**投递语义是至少一次**。handler 干完活后、确认前崩溃，消息会再来一次 —— 所以 handler 必须幂等。反过来（投递即确认）会在插件崩溃时丢活儿，而那正是这套机制要应对的情况。

topic 按插件隔离。插件 A 的 `emails` 和插件 B 的 `emails` 是两个队列。跨插件通信走事件总线，那里对越界是显式的。

### 缓存与锁

```go
err := sdk.Cache.Set(ctx, "profile:"+id, profile, 5*time.Minute)
found, err := sdk.Cache.Get(ctx, "profile:"+id, &profile)

lease, ok, err := sdk.Locks.Acquire(ctx, "nightly-job", 30*time.Second, 0)
if ok {
    defer lease.Release(ctx)
    // 长任务里定期续租；Renew 返回 false 说明租约已失去，
    // 别人可能已在做同样的活儿 —— 应当停下而不是继续
}
```

### 事件、文件、出站 HTTP

```go
// 事件：尽力而为的广播，订阅者跟不上就会丢。不能丢的走队列。
_ = sdk.Events.Publish(ctx, "note.created", note)
_ = sdk.Events.Subscribe(ctx, "otherplugin:thing.happened", handler)

// 文件：写入经插件，读取不经过 —— 下载链接给浏览器直接取
fileID, size, err := sdk.Files.Put(ctx, "report.pdf", "application/pdf", reader)
url, expires, err := sdk.Files.DownloadURL(ctx, fileID, userID, 5*time.Minute)

// 出站 HTTP：只能访问 egress_allow 里的域名
resp, err := sdk.HTTP.Get(ctx, "https://api.example.com/rates")
```

出站代理不只是匹配域名。它还会检查**实际拨号的 IP**：白名单上的域名可能解析到 127.0.0.1 或云元数据地址（169.254.169.254），无论是配置失误还是 DNS 重绑定攻击。同时不跟随重定向 —— 否则一个被允许的主机可以用 302 把你带去内网。

---

## Filter：介入请求生命周期

Filter 是 IIS Filter / ASP.NET HttpModule 那套模型：插件可以介入**任何**请求，不只是自己的。

| 阶段 | 时机 | 典型用途 |
|---|---|---|
| `pre_route` | 路由前，最早 | 限流、WAF、灰度 |
| `authenticate` | 建立身份 | 自定义认证 |
| `authorize` | 判定权限 | 细粒度授权 |
| `pre_handler` | 调后端前 | 改写路径、注入头 |
| `post_handler` | 后端返回后 | 改写响应 |
| `on_error` | 后端 5xx 或超时 | 兜底、告警 |
| `log` | 响应已发出 | 审计、埋点（异步，不影响响应） |

```go
sdk.Serve(sdk.Config{
    Filters: map[sdk.Phase]sdk.FilterFunc{
        sdk.PhasePreRoute: func(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
            if overLimit(req.ClientIP) {
                return sdk.Stop(429, []byte("slow down")).
                    WithHeader("Retry-After", "5"), nil
            }
            return sdk.Continue(), nil
        },
        sdk.PhasePreHandler: func(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
            return sdk.Mutate().
                SetRequestHeader("X-Tenant", tenantOf(req)).
                SetValue("tenant", tenantOf(req)), nil
        },
    },
})
```

### 失败策略

默认 **fail-open**：filter 超时或报错时请求照常放行。因为多数 filter 是观察者，一个坏掉的观察者不该让站点不可用。

任何做安全决策的 filter 必须显式声明 `fail_closed: true`，否则该插件宕机会悄悄变成一次鉴权绕过。

失败的 fail-open filter 仍会记录日志 —— 否则它会因为坏掉而恰好从运维视野里消失。

### Body

默认不把 body 跨进程传给 filter。实测一个 64KB 的 body 会让调用成本变成空 body 的四倍，而多数 filter 只看方法、路径、头和身份。

需要时声明 `needs_request_body: true` 并设 `max_body_bytes`。超过上限时：`fail_closed` 的 filter 返回 413，fail-open 的跳过。**不会截断后传给你** —— 一个基于半截数据做判断的安全 filter，可能得出和后端处理完整数据不同的结论。

### 身份改写

`SetIdentity` 只在插件持有 `filter:authenticate` 权限、**且**处于 authenticate/authorize 阶段时生效。

这两道门不是用来防恶意插件的（插件本就受信任），而是把「谁负责认证」这件事钉死：

- **权限门**让「这个插件会改写调用者身份」成为 manifest 里一句显式声明，审核时看得见，而不是藏在某个 filter 的代码里
- **阶段门**防的是顺序错误 —— 一个 log 阶段的 filter 改写身份时，鉴权早就跑完了，改了也只会让日志和实际放行的决策对不上

不满足条件时改写会被忽略并记一条日志，而不是静默丢弃。

### 成本

未订阅的路径**不产生任何跨进程调用**：

| 场景 | 成本 |
|---|---|
| 该阶段无人订阅 | 1.9 ns |
| 有 filter 但路径不匹配 | 8.2 ns |
| 真正调用一次 filter | ~37,000 ns |

所以 `match.paths` 写得越准，代价越低。写 `/**` 意味着每个请求都要跨进程一次。

---

## trace-id

Core 在入口生成或继承 trace id（优先 W3C `traceparent`，其次 `X-Request-Id`），它会跟着请求穿过每一个进程：filter、后端、你调用的每个 Host 能力、你投递的每条队列消息。

```go
sdk.Log.Info(ctx, "订单已创建", "order_id", id)
// [plugin:orders] INFO 订单已创建 trace=3473284c... order_id=...
```

你不需要手工传递。响应头也会带 `X-Request-Id`，用户报障时报给你的那串字符，能在 Core 日志、插件日志、慢查询记录里串起来。

异步任务也不例外：`Publish` 时当前 trace 会存进消息，`Consume` 时恢复为 parent trace，所以一个夜里跑的任务仍能追溯到白天触发它的那个请求。

---

## 信任模型

插件的定位和 IIS 的 ISAPI Filter 一样：它运行在宿主进程的权限之下，能力边界靠**安装前的审核**建立，而不是靠沙箱。

插件由 Core 作为子进程启动，与 Core **同用户、同文件系统权限**。这意味着：

- 它能读到 Core 这个用户能读的任何文件
- 它能占用 CPU 和内存，Core 不做配额限制
- 它进程崩溃不会拖垮 Core，但它可以做任何该用户能做的事

**所以插件必须先审核再安装。** 这是整套模型的前提，就像你不会往 IIS 里装一个来路不明的 filter。

### 哪些是真正强制的

以下检查跑在 **Core 进程内**，插件在连接的另一端，绕不过去：

| 机制 | 效果 |
|---|---|
| `permissions` 声明 | 未声明的 Host 能力调用直接 PermissionDenied |
| 数据命名空间 | 文档、缓存、队列、文件都按插件 key 隔离，用相同 key 也读不到别人的 |
| 事务归属 | 拿着别的插件的 `tx_id` 无法写入 |
| 出站白名单 | 只能访问 `egress_allow` 的域名，且拒绝解析到内网/元数据地址的目标 |
| SHA-256 校验 | 安装后二进制被改动一个字节即拒绝启动 |
| 环境隔离 | 插件读不到 `DATABASE_URL` 等，因而无法绕过 Core 直连数据库 |

最后一条不是防"偷密钥"——插件本来就是受信任的——而是防**架构漂移**：一旦插件能直连 PostgreSQL，Core 就不再拥有 schema、迁移和隔离的控制权，文档存储从"唯一路径"退化成"建议路径"。

### 哪些不是边界

- 文件系统：插件能读同用户可读的一切
- 资源：没有 cgroup 配额，一个死循环的插件会吃满 CPU
- 系统调用：没有 seccomp 限制

如果你确实要跑不受信任的代码，那属于容器层的问题（gVisor、独立容器、seccomp profile），不在这套插件模型的职责内。

### permissions 的真正价值

既然插件受信任，为什么还要声明权限？

两个理由，都跟"防恶意"无关：

1. **它是审核清单。** 你在批准一个插件时，一眼能看到它要队列和出站 HTTP，但不要文件写入。这比读代码快得多。
2. **它防误用。** 插件调了一个它本不该用的能力时会立刻失败，而不是悄悄改了数据 —— 这类 bug 在受信任的代码里同样会发生。

---

## 进程生命周期

Core 是插件的父进程，这带来两个必须知道的行为：

- **Linux 上设置了 `Pdeathsig`**：Core 崩溃时内核会杀掉插件。否则会留下孤儿进程占着套接字和内存，下次 Core 启动会撞上它们。
- **macOS 上没有这个机制**：本地开发时如果硬杀 Core，插件进程会活下来。看到端口被占时先检查有没有残留进程。

---

## 本地开发

```bash
# 起 Core（不带数据库也能跑，数据类能力会报 Unavailable）
PLUGIN_DIR=./plugins PLUGIN_DEV_MODE=1 go run ./core

# 改完插件后重新构建并在控制台点「重载」
CGO_ENABLED=0 go build -o plugins/notes/bin/plugin ./myplugin
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost/api/system/plugins/notes/upgrade
```

`PLUGIN_DEV_MODE=1` 会跳过 `Pdeathsig`，这样 air 重编译 Core 时不会连带冷启动所有插件。生产环境不要开 —— 没有 `Pdeathsig`，Core 崩溃会留下孤儿进程。

### 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `PLUGIN_DIR` | `./plugins` | 插件包目录 |
| `PLUGIN_DATA_DIR` | 空 | 每插件私有可写目录的根 |
| `PLUGIN_LOG_LEVEL` | `warn` | 插件日志级别 |
| `PLUGIN_DEV_MODE` | 关 | 跳过 Pdeathsig，仅开发用 |
