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
		fmt.Fprintf(w, "hello %s", sdk.User(r.Context()).Name())
	})
	sdk.Serve(sdk.Config{Handler: mux})
}
```

`sdk.User(ctx)` 在没有登录用户时返回 `nil`，所以用 `.Name()` / `.ID()` / `.HasRole()` 这些方法读取 —— 它们对 `nil` 安全。直接写 `.Username` 会在未认证请求上 panic，而**插件里的 panic 会杀掉插件进程**：一个匿名请求就能让这个插件下线。

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

Core 从插件 stdout 的**第一行**读取启动握手，所以 `sdk.Serve` 之前的一个 `fmt.Println` 会顶替掉握手，插件起不来。用 `sdk.Log` 或标准库 `log`（默认走 stderr，Core 会捕获）。

**好消息是这个错误会指着你的鼻子说**：go-plugin 会把它读到的那一行原样放进错误里。实测输出：

```
plugin noisy: handshake failed: Unrecognized remote plugin message: debugging: about to start
```

后半句就是插件自己打的那行。看到自己写的调试字符串出现在启动失败信息里，基本不用再查了。

**握手完成之后写 stdout 不致命** —— 实测插件继续正常服务，连续二十个请求无异常（`tests/stdout_test.go`）。所以这条规则实际是关于**启动阶段**的。但仍然不建议：go-plugin 会把子进程 stdout 转发进 Core 的日志，混在结构化日志里，而 `sdk.Log` 带 trace-id、能被过滤、能汇聚。

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
weight: 1                     # 副本权重，默认 1；见下方「多副本」

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
    icon: file-text           # 控制台用 lucide 图标名，见 lucide.dev
    order: 20
    roles: [admin]            # 非空时只有该角色可见（Core 侧过滤，不下发）
    entry: ""                 # 叶子节点留空 = 挂载本插件的微前端；
                              # 插件没有 frontend/ 目录时这个菜单点开是空白页，
                              # 纯后端插件就别声明 menus
    children:                 # 有 children 的节点是分组，不要给它 entry，
      - path: /notes/archive  # 否则控制台会试图把分组本身当页面挂载
        title: 归档
        order: 10

filters:
  - name: rate-limit
    phase: pre_route
    order: 10                 # 同阶段内跨插件排序，小的先跑
    match:
      paths: ["/api/**"]      # glob：精确路径、/前缀/**、/a/*/c
      methods: ["POST", "PUT"]
    timeout_ms: 50
    fail_closed: false        # 见下方「失败策略」
    needs_request_body: false  # 见下方「Body」
    needs_response_body: false # post_handler/on_error 想读响应体时必须开
    max_body_bytes: 65536

config:                       # 管理员能配什么，见下方「配置」
  - key: retention_days
    label: 保留天数            # 控制台上的字段名，省略则用 key
    description: 超过这个天数的记录会被清理
    type: int                 # string | int | bool | duration | text
    default: "30"             # 未设置时 Core 补给插件的值
    required: false
    secret: false             # true 时控制台隐藏取值

jobs:
  - name: nightly-summary
    cron: "17 3 * * *"        # Core 调度，多副本只跑一个，禁用即停

egress_allow:                 # 出站 HTTP 白名单
  - api.example.com
  - "*.cdn.example.net"
```

### 运行时的上限

上面那些管的是 manifest 能声明什么，这些管的是插件运行时能占多少 —— 都是共享资源，一个插件用光了别人就没有：

| | 上限 | 为什么 |
|---|---|---|
| 同时打开的事务 | 每插件 4 个 | 每个占一条数据库连接 |
| 队列积压 | 每插件 10 万条待处理 | 队列是共享的表和磁盘 |
| 同时持有的锁 | 全局 1 万个 | 过期的锁也会定期清理 |
| 事件订阅 | 每插件 64 路 | 每路是一条常驻的流 |
| 数据库连接池 | 全局 25 条 | 低于 PostgreSQL 默认的 max_connections |

除连接池外都是**按插件**计的，这是有意的：一个插件的失误应该是那个插件的错误，而不是所有人的故障 —— 而且错误信息里会写明是哪个插件。超限是一个**状态**不是惩罚：占用降下来就恢复。

### 声明的上限

manifest 里的东西不是无限的。这些上限比任何合理用法都宽一到两个数量级，存在的理由不是防恶意（插件本来就是可信的），而是让**一次失误**在安装时变成一条明确的错误，而不是变成一个跑起来莫名其妙很慢的 Core、一个多了一千张表的数据库，或者一棵浏览器渲染不动的菜单树：

| | 上限 |
|---|---|
| manifest 文件本身 | 1 MB |
| filters | 64 |
| database.collections | 64 |
| jobs | 64 |
| config | 128 |
| 菜单深度 | 8 层 |
| 菜单节点总数 | 256 |

Core **不接受它不认识的字段**。`filter:` 少写一个 s、`permission:` 少写一个 s、或者照着某份旧设计文档写了个 `resources:`，都会在加载时直接报错，指出是哪一行。

这条规则的理由不是洁癖。manifest 是审核的人用来决定装不装的东西，也是 Core 真正执行的东西 —— 一个被静默丢弃的字段会让这两者不一致。多数拼写错误碰巧是安全的（`permission:` 拼错等于没申请权限，插件第一次调用就会失败），但 `filters:` 拼错不是：一个整个存在意义就是 fail-closed 鉴权 filter 的插件会正常安装、显示为运行中，而每个请求都不经鉴权就过去了，屏幕上还挂着那份写着相反内容的 manifest。

还有一个不是数字的理由：manifest 是运维批准这个插件之前要读的东西，一份 6MB 的 manifest 没人读得动。

---

## Host 能力

所有能力在 `sdk.Serve` 启动后即可用，包括插件自己的初始化阶段。每次调用都由 Core 按 manifest 校验权限。

插件自己的身份和落盘位置也从 SDK 拿，不用自己解析环境变量：

```go
sdk.Key()      // manifest 里的 key
sdk.DataDir()  // 本插件私有的可写目录；文件系统其余部分都当只读
```

### 文档存储

```go
// 写入
version, err := sdk.DB.Put(ctx, "notes", id, note)

// 乐观锁：版本不匹配返回匹配 sdk.ErrVersionConflict 的错误，重读后重试
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

// 事务（需要 db:tx）。TxClient 的方法和非事务版本一一对应，
// 签名也一样 —— 包括 Get 返回 version、以及 PutIfVersion。
err := sdk.DB.Tx(ctx, 30*time.Second, func(tx *sdk.TxClient) error {
    var stock Stock
    found, version, err := tx.Get(ctx, "stock", sku, &stock)
    if err != nil {
        return err
    }
    if !found || stock.OnHand < qty {
        return errNotEnough   // 从闭包返回的错误会原样传出 Tx，可以用 errors.Is 判断
    }

    stock.OnHand -= qty
    // 事务内也要用乐观锁：见下面「并发下要重试的两种失败」
    if _, err := tx.PutIfVersion(ctx, "stock", sku, stock, version); err != nil {
        return err
    }
    if _, err := tx.Put(ctx, "orders", orderID, order); err != nil {
        return err
    }
    return tx.Delete(ctx, "cart", cartID)
})
```

完整的查询方法：

| 条件 | 说明 |
|---|---|
| `Eq` `Ne` | 等于 / 不等于 |
| `Gt` `Gte` `Lt` `Lte` | 大小比较 |
| `Like` | SQL LIKE，`%` 通配 |
| `In(field, v...)` | 属于集合 |
| `Between(field, lo, hi)` | 闭区间 |
| `IsNull` `IsNotNull` | 字段是否存在 |
| `Sort` `SortDesc` | 排序，可叠加多个 |
| `Limit` `After(cursor)` | 分页 |

终结方法：`All(ctx, &dest)` 取一页并返回下一页游标，`Count(ctx)`，以及 `Sum` / `Avg` / `Min` / `Max`（都可选传 group by 字段）。

单条操作：`Put`、`PutIfVersion`、`Get`、`Delete(ctx, collection, id) (found bool, err error)`。删除不需要事务；`Delete` 返回的第一个值告诉你那条记录本来是否存在。

**所有比较值都是字符串**，包括数字和时间。这是因为文档存储的字段类型由 JSON 决定，而比较需要一个确定的顺序。实际影响是：**要按时间范围查询，就得把时间存成按字典序可比的格式** —— RFC3339（`2026-08-16T03:17:00Z`）可以，Unix 时间戳整数不行（`"9"` 排在 `"10"` 后面）。

```go
// 可以：字典序等于时间序
Gt("created", "2026-01-01T00:00:00Z")

// 不行：字符串比较下 "999" > "1000"
Gt("created_unix", strconv.FormatInt(ts, 10))
```

**分页用游标不用 offset**。offset 会让数据库逐行跳过并丢弃，翻得越深越慢，而且期间有行插入或删除会导致重复或遗漏。游标基于排序键加主键做行值比较，翻到第一百页和第一页一样快。代价是所有排序字段必须同方向 —— 方向混用会被拒绝，而不是悄悄返回错误的分页。

**事务有超时，也有数量上限**。它占着一个数据库连接，所以插件崩溃时 Core 会到期回滚（默认 30 秒，最长 5 分钟），不会永久占用。同时**每个插件最多同时开 4 个事务** —— 超出会被直接拒绝，而不是排队等连接池。

这两条都指向同一件事：**把事务里的活儿写短**。需要同时开四个以上事务的场景，通常真正想要的是一个事务，或者根本不需要事务。

上限是按插件算的，所以一个插件用光自己的额度不会影响别的插件 —— 否则一个插件的失误就是所有人的故障。

### 持久化队列

```go
// 发布
id, deduped, err := sdk.Queue.Publish(ctx, "emails", payload,
    sdk.WithDelay(5*time.Minute),
    sdk.WithDedupKey("order-"+orderID),
    sdk.WithMaxAttempts(3),
)

// 消费：返回 nil 即确认，返回 error 即重试
//
// Consume 和 Subscribe 一样是阻塞的 —— 它一直收到 ctx 取消或出错才返回，
// 所以要自己起 goroutine，并且不要放在 sdk.Serve 之后（那行不会执行到）。
// 放在 OnConfigChanged 里用 sync.Once 起，或者在 Serve 之前起。
go func() {
    if err := sdk.Queue.Consume(consumerCtx, "emails", handleEmail); err != nil {
        sdk.Log.Error(consumerCtx, "email consumer stopped", "err", err)
    }
}()

err := sdk.Queue.Consume(ctx, "emails", func(ctx context.Context, m *sdk.QueueMessage) error {
    var job EmailJob
    if err := m.Decode(&job); err != nil {
        return err  // 重试到上限后进死信
    }
    return send(job)
})
```

handler 收到的 `*sdk.QueueMessage` 长这样 —— 给出类型而不是名字列表，因为**一份散文式的字段清单只会招来类型猜测**：一位只读文档的作者据此把 `ID` 当成了字符串，三处编译错误。

```go
type QueueMessage struct {
    ID            int64    // 这次投递的消息 id，去重和记账用
    Topic         string
    Payload       []byte   // 或者用 m.Decode(&dest) 直接反序列化
    Attempt       int      // 这是第几次投递，从 1 开始
    MaxAttempts   int
    ParentTraceID string   // 入队时那个请求的 trace
}
```

**投递语义是至少一次**。handler 干完活后、确认前崩溃，消息会再来一次 —— 所以 handler 必须幂等。反过来（投递即确认）会在插件崩溃时丢活儿，而那正是这套机制要应对的情况。

**重投不是立刻的。** 消息投递给消费者时会被租约锁住（默认 30 秒可见性超时），消费者消失后要等两件事：租约到期，以及 Core 的队列维护循环把它收回可投递状态（每 30 秒一轮）。所以最坏情况下，一条被崩溃的插件带走的消息，最多约一分钟后才会重新出现。这个延迟是设计的一部分 —— 缩短它意味着更容易把「慢」误判成「死」，从而让两个消费者同时处理同一条消息。

需要更快回来的任务，在 `Consume` 时把可见性超时调小；那等于声明「这个 handler 一定在 N 秒内完成，否则就当它死了」。

**积压有上限**：每个插件最多 10 万条待处理消息，超出后 `Publish` 直接报错。队列是一张共享的表、放在一块共享的磁盘上，所以一个陷入循环的生产者撑爆的是所有人的磁盘，不只是它自己的。

这个数字定得很高，因为一次合法的批量作业本来就大；它挡的是「完全没有上限」那种情况。上限按插件算，所以别的插件不受影响；积压降下来之后自动恢复接受 —— 那是一个状态，不是惩罚。

深度是后台每 30 秒测一次的，不是每次入队都数一遍：让常见路径为罕见故障买单不划算，而且一个已经失控的生产者，早几秒发现并不会好多少。

topic 按插件隔离。插件 A 的 `emails` 和插件 B 的 `emails` 是两个队列。

**推论值得说明白：队列只能自产自消。**没有办法消费别的插件投进来的消息 —— 一个"处理别人交办的活儿"的架构，用队列是搭不出来的。跨插件通信走事件总线，那里对越界是显式的；但事件是尽力而为的（订阅者跟不上就丢），所以「跨插件 + 不能丢」目前没有现成答案：接收方要自己在事件 handler 里立刻投进**自己的**队列，用一次内存传递换取之后的持久保证。

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

// 订阅是阻塞的：它一直收到 ctx 取消或出错才返回，所以要自己起 goroutine
go func() {
    if err := sdk.Events.Subscribe(ctx, "otherplugin:thing.happened", handler); err != nil {
        sdk.Log.Error(ctx, "订阅结束", "err", err)
    }
}()

// 文件：写入经插件，读取不经过 —— 下载链接给浏览器直接取
fileID, size, err := sdk.Files.Put(ctx, "report.pdf", "application/pdf", reader)
url, expires, err := sdk.Files.DownloadURL(ctx, fileID, userID, 5*time.Minute)

// 出站 HTTP：只能访问 egress_allow 里的域名
resp, err := sdk.HTTP.Get(ctx, "https://api.example.com/rates")
```

`Subscribe` 和 `Publish` 长得像一对，但行为完全不同：`Publish` 发完就返回，`Subscribe` 是一个收到流结束才退出的循环。直接写在 `main()` 或某个初始化函数里会把插件挂在那里。

出站代理不只是匹配域名。它还会检查**实际拨号的 IP**：白名单上的域名可能解析到 127.0.0.1 或云元数据地址（169.254.169.254），无论是配置失误还是 DNS 重绑定攻击。同时不跟随重定向 —— 否则一个被允许的主机可以用 302 把你带去内网。

出站失败会带上可区分的 gRPC 状态码，因为这四种情况的处理方式完全不同：域名不在 `egress_allow` 里是 `PermissionDenied`（永久的，要改 manifest 并重新审核）、超出速率是 `ResourceExhausted`（等一下就好）、URL 拼错是 `InvalidArgument`（你的 bug）、对端连不上是 `Unavailable`（可以重试）。在此之前它们全都是 `Unknown`，一个字符串，无从分支。

同理，对不存在的文件调 `DeleteFile` 或 `GenerateDownloadToken` 拿到的是 `NotFound` 而不是 `Internal`。`GetFileMetadata` 不同 —— 它问的是「存不存在」，所以返回 `Found: false` 而不是错误。另一个插件的文件与不存在的文件在这三个调用上完全一样，这是故意的：能确认某个 id 真实存在，正是探测想要的东西。

### 插件不可用时调用者看到什么

| 情况 | 状态码 | 含义 |
|---|---|---|
| 插件被停用 / 未安装 / 已隔离 | **404** | 这条路由不存在，和菜单消失是同一件事。别重试 |
| 插件在启动、排空、或名额已满 | **503** | 中间状态，重试是对的 |
| 插件在，但这次调用失败了（崩溃、超时） | **502** | 上游坏了 |

这三种以前全都是 502。实测停用一个正在服务的插件：5801 个请求全部 502 —— 客户端会当成故障去重试，盯着 502 告警的人会被一次**主动的运维操作**叫醒。

### 测试

插件的 handler、事务逻辑、filter 判断都是普通 Go 函数，用普通的 `go test` 测，不需要跑起 Core。

**测 filter**：直接调用它，用 `Inspect()` 看它决定了什么。

```go
res, err := myAuthFilter(ctx, &sdk.FilterRequest{
    Phase:  sdk.PhaseAuthenticate,
    Method: "GET",
    Path:   "/api/plugins/notes/items",
    Header: http.Header{},   // 没带凭据
})

got := res.Inspect()
if got.Action != sdk.ActionStop || got.Status != 401 {
    t.Errorf("匿名请求没有被拒绝：%+v", got)
}
```

`Inspect()` 返回的 `FilterDecision` 里有全部内容：`Action`（`ActionContinue` / `ActionStop` / `ActionMutate`）、短路时的 `Status` / `Body` / `Header`、mutate 时的 `Identity` / `Path` / `SetRequest` / `Values`。

**测 handler 的鉴权**：用 `sdk.WithUser` 造一个带调用者的 context。

```go
req := httptest.NewRequest("POST", "/keys", nil)
req = req.WithContext(sdk.WithUser(req.Context(), &sdk.UserContext{
    UserID: "1", Username: "root", Roles: []string{"admin"},
}))
```

传 `nil` 就是匿名请求 —— **这个用例一定要测**：`sdk.User(ctx)` 会返回 `nil`，而插件里的 panic 不是 500 而是进程死亡。

**还没有的**：一个能替 `sdk.DB` / `sdk.Queue` / `sdk.Cache` 的内存实现。碰到这些能力的代码，目前只能靠把它和纯逻辑分开来测，或者照本仓库 `tests/` 的做法 —— 现场编译插件、由 Core 启动、发真实请求。这是一个已知的空缺。

### 一次响应能有多大

插件和 Core 之间是 gRPC，单条消息上限 **16 MiB**（gRPC 自己的默认是 4 MiB，两端都显式抬高了，并在 `Configure` 时协商）。

超了会怎样：插件的 `HandleHTTP` 返回一个 `ResourceExhausted` 错误，错误里带着两个数字（实际大小 vs 上限）；调用者拿到 **502**，正文里就是这句话。**插件进程不会因此死掉**，下一个请求照常。

不是 413 —— 413 说的是「你发来的请求体太大」，而这是响应太大，调用者改什么都没用。

真正的大块内容不要塞进响应体，走文件能力：插件用 `sdk.Files.Put` 写入，再给一个 `DownloadURL`，浏览器直接从 Core 取。二进制内容本来就不该穿过插件传输层。

### 几件文档以前没说的小事

**路由是相对的。**Core 会剥掉 `/api/plugins/<key>` 前缀再交给你的 `http.Handler`，所以 `mux.HandleFunc("GET /orders", ...)` 对应外部的 `/api/plugins/orders/orders`。而 filter 的 `match.paths` 是在 Core 那一层匹配的，要写**完整路径**。

**它是一个真正的 `*http.Request`。**Go 1.22 的 `mux.HandleFunc("DELETE /keys/{id}", ...)` 和 `r.PathValue("id")` 都正常工作。

**你传给 `Put` / `Publish` 的值会被 SDK 用 `encoding/json` 编码**，不需要自己 `json.Marshal`；`Get` 和 `m.Decode` 反过来。所以文档字段名来自 `json:` tag。

**`sdk.Cache` 有 `Delete`**，不只是 `Get`/`Set`。

**`config` 里的 `required: true` 不强制任何东西。**Core 不会因为它没填就拒绝启动 —— 那会把一个漏填变成一次故障 —— 它只是让控制台标出来、让审核的人看见。所以插件代码里该做的防御一样要做。

**`OnShutdown` 能用多久**：Core 用的是排空超时（`DrainTimeout`，默认 30 秒），`OnShutdown` 在这段预算之内。写一个比它短的内部超时，别让 `wg.Wait()` 无限期挂住。

### 同一个阶段挂多个 filter

manifest 里 `filters:` 是列表，同一个 phase 可以有多条 —— 不同的 `match`、不同的 `fail_closed`。Go 侧的 `Filters` 是 `map[sdk.Phase]sdk.FilterFunc`，一个 phase 一个函数，所以这几条声明**都会走进同一个函数**。用 `req.Name` 区分：

```go
sdk.PhasePreRoute: func(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
    switch req.Name {          // manifest 里那条声明的 name
    case "ratelimit":
        return checkRate(ctx, req)
    case "waf":
        return checkPayload(ctx, req)
    }
    return sdk.Continue(), nil
},
```

不要靠 `req.Path` 反推是哪条匹配的 —— 那是在重做 Core 已经做过的匹配，而且两条声明同时匹配同一个路径时根本推不出来。

### 收尾：OnShutdown

`Consume` 和 `Subscribe` 起的 goroutine 会一直跑到 ctx 被取消。插件被停用、升级或排空时，Core 会先调 `OnShutdown`，之后才杀进程：

```go
consumerCtx, stopConsumers := context.WithCancel(context.Background())

sdk.Serve(sdk.Config{
    Handler: mux,
    OnShutdown: func(ctx context.Context) error {
        // 先让长期 goroutine 停下来，再返回。返回之后 Core 才继续排空。
        stopConsumers()
        return nil
    },
})
```

不写它也不会出错 —— 进程终究会被杀掉 —— 但一条正在处理的队列消息会因此没有 ack 也没有 nack，要等租约超时（默认 30 秒）才会被重投。写了它，那条消息立刻回到队列。

### 用非 session 的方式鉴权

`authenticate` 阶段的 filter 可以调用 `SetIdentity` 告诉 Core 调用者是谁 —— API key、JWT、mTLS、签名请求，任何 Core 自己不认识的凭据都走这条路。前提是插件声明了 `filter:authenticate`，否则 Core 会丢弃这个 mutation 并记一行日志。

顺序是这样的：

1. Core 先解析自己的 session。解析不出来**不是拒绝**，只是这个请求暂时匿名。
2. `authenticate` 阶段的 filter 依次跑，可以设置或替换身份。
3. `authorize` 阶段的 filter 跑，可以短路拒绝。
4. 到这里身份仍然是空的话，Core 返回 401。
5. `pre_handler`、后端、`post_handler`。

第 1 步和第 4 步是分开的：session 解析不出来**不是拒绝**，只是这个请求暂时匿名，第 4 步才是那个无条件的 401。这两步之间就是 authenticate filter 的机会窗口 —— 没有它，一个带着 API key 但没有 session cookie 的请求会在懂它凭据的插件被问到之前就被拒掉。

**一个必须知道的限制**：插件路由不能是公开的。第 4 步是无条件的，所以一个 `authorize` filter 返回 Continue 并不能放行匿名请求。没装 authenticate filter 的部署行为和以前完全一致。

[`apikey` 示例](../extension-example/apikey)是这条路的完整写法。

```go
sdk.Serve(sdk.Config{
    Filters: map[sdk.Phase]sdk.FilterFunc{
        sdk.PhaseAuthenticate: func(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
            key := req.Header.Get("Authorization")
            user, ok := resolve(ctx, key)
            if !ok {
                // 认不出来就让它匿名过去。拒绝是 authorize 阶段的事 ——
                // 这个 filter 不知道哪些路由是公开的。
                return sdk.Continue(), nil
            }
            return sdk.Mutate().SetIdentity(&sdk.UserContext{
                UserID:   user.ID,
                Username: user.Name,
                Roles:    user.Roles,
            }), nil
        },
    },
})
```

`SetIdentity` 收的是 `*sdk.UserContext`，不是一个字符串 id —— 角色要一起给，因为下游的 `HasRole` 和 `authorize` 阶段读的就是它。`req.Identity` 也是同一个类型，且对 `nil` 安全。

### `sdk.Cache` 是一次 RPC，不是本地缓存

缓存在 Core 里，所以一次「缓存命中」仍然是一次跨进程往返。用 [`apikey` 示例](../extension-example/apikey)实测每次调用：

| | 每次调用 |
|---|---|
| 无凭据（最便宜的路径） | 39 µs |
| 只用 `sdk.Cache` | 124 µs |
| 自己在进程内再缓一层 | 43 µs |

对绝大多数用途这不重要 —— 一个 handler 里省掉一次数据库查询，80µs 换几毫秒是划算的。**但 filter 不同**：一个订阅 `/**` 的 authenticate filter 在每个请求上跑，所以那 80µs 加在系统里的每一个请求上，包括属于完全无关插件的请求。

热路径上的做法是两层：进程内一层短 TTL 挡在前面，`sdk.Cache` 作为跨副本共享的那层。代价是撤销/失效会慢一点 —— 本进程会继续用旧值直到自己的条目过期，而别的副本清不掉它。所以那个短 TTL 配的其实是**一个已失效的东西最长还能被用多久**，应该单独可配而不是和共享层共用一个数字。

### 队列放弃一条消息的时候

handler 返回错误时 SDK 会 nack，Core 按 `max_attempts`（默认 5）重投；用完之后消息被标记为 **dead**，不再投递。这是对的 —— 一条毒消息无限重试比放弃更糟 —— 但要知道它意味着什么：**那条工作已经被接收，并且永远不会完成了**。

对插件作者：handler 拿不到「这是最后一次尝试」的通知，只能自己看 `msg.Attempt` 和 `msg.MaxAttempts`。真正不能丢的东西，要在最后一次尝试时写到别处去（另一个集合、一条告警），而不是指望有人来捞。

```go
sdk.Queue.Consume(ctx, "summaries", func(ctx context.Context, m *sdk.QueueMessage) error {
    if err := doWork(m); err != nil {
        if m.Attempt >= m.MaxAttempts {
            // 最后一次了。再返回错误这条消息就消失了。
            recordFailure(ctx, m, err)
        }
        return err
    }
    return nil
})
```

对运维：Core 现在会在放弃时记一行日志，控制台的插件列表也会显示「已放弃 N 条」。在此之前这件事完全没有出口 —— 而且更坏的是，控制台显示的队列积压只数 pending 和 processing，所以**一个 handler 永久损坏的插件把整个积压排空进 dead 之后，看起来就像刚追平了进度**。

### 并发下要重试的两种失败

Core 用 gRPC 状态码回答，但插件不该为了判断一个错误去 import `google.golang.org/grpc`。SDK 因此提供哨兵，用 `errors.Is` 判断：

| 哨兵 | 含义 | 该怎么做 |
|---|---|---|
| `sdk.ErrVersionConflict` | 别人先写了，你手上的版本是旧的 | 重读、重算、重试 |
| `sdk.ErrTxExpired` | 事务已经没了（提交过、回滚过、或超时） | 重试这次写没有意义，整个事务要重来 |
| `sdk.ErrRateLimited` | 撞上了上限（出站速率、事务名额、队列深度） | 退避后重试 |
| `sdk.ErrNotAllowed` | 权限或 `egress_allow` 不允许 | 重试没用，要改 manifest 并重新审核 |
| `sdk.ErrNotFound` | 文档、文件或消息不存在 | — |

前两个以前都是 `FailedPrecondition`，一个重试循环分不清「这次写再试一次」和「这个事务已经不在了」，会一直转。

**事务保证的是原子性，不是不争抢。**两个事务能读到同一行，第二个写会被拒绝而不是覆盖掉第一个 —— 所以 `ErrVersionConflict` 在竞争下是正常结果。要重试的是**整个事务**（余量已经变了，判断得重做），不是那一次写。

另外，一个事务占着一条数据库连接，所以 Core 限制单插件同时持有的事务数（默认 8）。撞上返回 `ErrRateLimited`。把事务放在热路径上就会遇到它，这不是故障。

这个 8 是量出来的，不是拍的。32 个并发事务、每秒完成数：

| 上限 | 1 | 2 | 4 | 8 | 16 | 25 |
|---|---|---|---|---|---|---|
| 各写各的文档 | 713 | 920 | 1257 | **1683** | 1567 | 716 |
| 都抢同一行 | 410 | 346 | 273 | 187 | 132 | 128 |

第一行是插件通常的样子，峰值在 8；再往上连接池（25 条）开始见底，16 时放弃数翻两番，25 时吞吐腰斩。第二行是所有事务抢同一行，上限越高越差 —— 多出来的并发全变成行锁等待和版本冲突。热点行无论如何都会被 PostgreSQL 限住，所以取了第一行的峰值。

[`inventory` 示例](../extension-example/inventory)把这两条都做了，README 里有 10 件库存被 30 个并发请求争抢时，加与不加重试的实测数字。

### 升级时的数据形状

Core 建表、建索引，但**不会改写已有文档**。`database.collections` 是加法的：新集合会被创建，新索引会被创建，而一个从存 `name` 改成期望 `full_name` 的插件，升级后面对的是一整批仍然带着 `name` 的旧文档，Core 不会替你动它们。

所以字段改名这类事目前是插件作者的责任。两种做法：

- **读时兼容**：新版本同时认 `full_name` 和 `name`，写的时候只写 `full_name`。简单，但兼容代码会一直留着。
- **启动时迁移**：在 `OnConfigChanged` 之外自己跑一次批量 `Query` + `Put`。要自己保证跑第二遍是安全的 —— 升级、重启、多副本都会让它不止跑一次。

Core 里有一套声明式迁移（`rename_field` / `drop_field` / `set_default`）已经写好并测过，但**没有接线**：manifest 里没有地方声明它，Core 也没有记录哪些已经跑过。在它接上之前，上面两条是全部选项。

### 配置

**先在 manifest 里声明你接受哪些配置项**，再在代码里读。不声明也能读到管理员随手填的键，但那样键名就成了你和运维之间的口头约定 —— 任何一边拼错，插件都会安静地用编译内的默认值跑下去，而控制台上显示着管理员以为生效了的值，两边都看不出问题。

声明之后：控制台按声明渲染表单而不是自由文本编辑器；`default` 由 Core 补齐，所以你的 `OnConfigChanged` 拿到的 map 总是完整的，不用在代码里再写一遍默认值；审核插件的人能一眼看到它可以被配置成做什么。

管理员改动后 Core 立即推送给运行中的进程，**不需要重启插件**。

```go
sdk.Serve(sdk.Config{
    Handler: mux,
    OnConfigChanged: func(cfg map[string]string) {
        limiter.reconfigure(cfg)   // 启动时调用一次，之后每次变更再调用
    },
})
```

`OnConfigChanged` 在两个时机触发：插件启动时带着管理员当前配置调用一次，之后每次管理员改动再调用。**所以配置只需要一条代码路径**，不用分「启动时读一次」和「变更时更新」两套逻辑 —— 那两套迟早会不一致。

后续的调用发生在后台 goroutine 上，与请求并发，所以改动共享状态要加锁。

有一个陷阱值得单独说：

```go
func main() {
    cfg := sdk.GetConfig()      // 永远是空的
    lim := newLimiter(cfg)
    sdk.Serve(sdk.Config{...})
}
```

配置来自 Core，而 Core 是在 `sdk.Serve` 之后才发过来的。在 `Serve` 之前调用 `sdk.GetConfig()` 必然拿到空 map，插件于是悄悄用编译内的默认值运行，而控制台上显示的是管理员设的值 —— 两边都看不出问题。`sdk.GetConfig()` 只在请求处理期间有意义；配置初始化一律走 `OnConfigChanged`。

值仍然一律是字符串 —— `type` 只影响控制台用什么输入框，因为 manifest 说了不算，实际生效的是管理员键入的东西。所以解析仍然要做，而且**无法解析时要回落到默认值，不要关掉这项功能**：控制台上的一个笔误不应该变成一扇敞开的门。

`secret: true` 的项在控制台上显示为一串圆点，读取接口也只返回圆点。管理员保存表单时把圆点原样交回来，Core 理解为「这一项不动」—— 否则改一个无关字段就会把真凭据覆盖成那串圆点，而下次插件用它的时候才会发现，那时原值已经没了。要换掉它，键入新值即可。

管理员设的值存在 Core 的数据库里。**Core 在没有 `DATABASE_URL` 时无处可存**，配置接口会明说这件事而不是收下再丢掉 —— 那种失败会成功一次，刚好够让人相信它。这种部署下生效的只有 manifest 里的 `default`。

管理员显式清空一个字段时，你收到的是空字符串而不是声明的默认值 —— 清空是一个决定，Core 不会替他们改回去。

完整例子见 [`extension-example/ratelimit`](../extension-example/ratelimit)。

### 日志与指标

```go
sdk.Log.Info(ctx, "订单已创建", "order_id", id, "total", total)
sdk.Log.Error(ctx, "支付回调失败", "err", err, "attempt", n)
sdk.Log.Metric(ctx, "orders_created", 1, map[string]string{"region": region})
```

字段是交替的键值对，值可以是任意类型 —— 字符串、数字、error 都行，Core 侧统一格式化。trace id 自动附加，不用手工传。

### 插件自己的鉴权

**`menus` 里的 `roles` 不保护你的接口。** 它只决定这个菜单项在控制台上显示给谁 —— Core 在下发菜单树时过滤掉，仅此而已。任何人只要拼得出 URL，就能直接请求 `/api/plugins/<key>/...`，菜单上看不看得见毫无关系。

接口的鉴权在你自己的 handler 里做 —— 但先看清它的适用范围：

**handler 里的检查只对能拿到 Core session 的调用者有意义。**没有 session 的请求根本到不了你的 handler：Core 在 authenticate 和 authorize 阶段之后、进 handler 之前会无条件 401（见下面「用非 session 的方式鉴权」）。所以 handler 里的 `HasRole` 判的是「这个已登录的人能不能做这件事」，而「这个人是谁」要么来自 Core 的 session，要么来自某个 authenticate 阶段的 filter。两件事，两个地方。



```go
mux.HandleFunc("GET /entries", func(w http.ResponseWriter, r *http.Request) {
    if !sdk.User(r.Context()).HasRole("admin") {
        http.Error(w, "forbidden", http.StatusForbidden)
        return
    }
    // ...
})
```

`HasRole` 对 `nil` 安全，所以未认证的调用者自然被拒。需要更细的判断就读 `sdk.User(ctx)` 的字段，或者用 `authorize` 阶段的 filter 做集中式判定。

### 定时任务

manifest 里 `jobs:` 声明的每个任务，在代码里注册一个同名 handler：

```go
sdk.Serve(sdk.Config{
    Handler: mux,
    Jobs: map[string]sdk.JobFunc{
        "nightly-summary": func(ctx context.Context, job *sdk.Job) error {
            // job.Scheduled 是这次运行「本该」发生的时刻（Unix 秒）
            window := time.Unix(job.Scheduled, 0)
            return summarise(ctx, window)
        },
    },
})
```

名字对不上的不会被调用：manifest 里声明了但这里没注册的任务，Core 调过来时没有 handler；这里注册了但 manifest 没声明的，永远不会被触发。

**用 `job.Scheduled`，不要用 `time.Now()`。** 它是这次运行对应的那个计划时刻。Core 忙、插件刚重启、任务排队，都会让实际执行时间晚于计划时间；一个按「昨天」汇总的任务如果用 `time.Now()`，在 00:03 跑就会汇总错日期，而且这种错只在延迟发生的那天出现。

调度归 Core：插件被禁用任务就停，多副本下一次只有一个副本执行，不需要自己加分布式锁。

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

`log` 阶段对**每一个**请求都会跑，包括被更早的 filter 短路掉的、鉴权失败的、以及后端返回 5xx 的 —— 所以审计不会对「被拒绝的流量」留下盲区。它在响应发出之后异步执行，返回值被忽略，`fail_closed` 在这里没有意义（响应都发完了，拒无可拒）。

**这带来一个用「审计」当例子时必须讲清楚的后果**：`log` 阶段的 filter 失败时，请求已经成功了，而那条审计记录就这么没了 —— 插件崩溃、熔断打开、正在排空的那段时间都会发生，只在 Core 自己的运维日志里留下「这个 filter 失败了」。也就是说**基于 `log` 阶段的审计是尽力而为的，不保证完整**。真正不能丢的审计要在 `log` 阶段里把记录投进持久队列（那有至少一次投递保证），或者接受这个缺口并监控 Core 日志里的 filter 失败。

filter 收到的 `*sdk.FilterRequest` 字段：

| 字段 | 何时有值 |
|---|---|
| `Phase` `TraceID` `Method` `Path` `Query` `ClientIP` `Header` | 一直有 |
| `Identity` | Core 解析出登录用户时；未认证是 `nil` |
| `Body` | 仅当声明了 `needs_request_body` |
| `ResponseStatus` `ResponseHeader` | `post_handler`、`on_error`、`log` 阶段 |
| `ResponseBody` | 上述阶段且声明了 `needs_response_body` |
| `Values` | 同一请求里更早的 filter 用 `SetValue` 放进去的 |

`sdk.User(ctx)` 在 filter 里同样可用，读到的是 Core 解析出的调用者 —— 和 `req.Identity` 是同一份。

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

`SetValue` 和 `SetRequestHeader` 的去向不一样，容易混。`SetValue` 存的东西只能被**同一请求里更晚的 filter** 用 `sdk.Values(ctx)` 读到；它不会传给最终处理这个请求的 backend handler —— 在自己的 HTTP handler 里 `sdk.Values(ctx)` 永远是空的。要把信息交给 backend，用 `SetRequestHeader`：那是请求的一部分，handler 从 `r.Header` 就能读到。

### 失败策略

默认 **fail-open**：filter 超时或报错时请求照常放行。因为多数 filter 是观察者，一个坏掉的观察者不该让站点不可用。

**能自己放行请求的安全 filter 必须声明 `fail_closed: true`**，否则该插件宕机会悄悄变成一次鉴权绕过。

界线是「这个 filter 宕掉，请求会不会因此被放过」。一个 `authorize` 阶段、负责拒绝的 filter：宕了就没人拒绝，必须 fail-closed。一个 `authenticate` 阶段、只负责贴身份标签、从不自己放行的 filter：宕了的后果是有效凭据的调用者被降级成匿名，后面的 `authorize` 和 handler 该拒还是拒 —— 那是**更严格**而不是更松，fail-open 反而是对的（`fail_closed: true` 会把它变成「认证服务一抖，全站 503」）。

判断方法：把这个 filter 想成永远返回 `Continue`，问一句「有没有本该被拒的请求因此通过了」。答案是「有」就 fail-closed。

声明了 `fail_closed` 时，filter 无法给出判断的请求一律返回 **503**，包括这几种情形：插件没在运行、正在排空、熔断器已打开、以及这次调用本身报错或超时。对调用方来说它们是同一件事 —— 这个 filter 没能做出决定 —— 所以状态码也是同一个。

失败的 fail-open filter 仍会记录日志 —— 否则它会因为坏掉而恰好从运维视野里消失。

**熔断**：连续失败会打开该插件的熔断器，之后一段时间内它的 filter 直接跳过，不再付跨进程调用的钱。熔断器是**按插件**的，一个插件坏掉不会影响别的插件的 filter。fail-open 的 filter 熔断后请求继续放行，fail-closed 的继续拒绝 —— 熔断改变的是「要不要再问它」，不是「问不到时怎么办」。

### Body

默认不把 body 跨进程传给 filter。实测一个 64KB 的 body 会让调用成本变成空 body 的四倍，而多数 filter 只看方法、路径、头和身份。

需要时声明 `needs_request_body: true` 并设 `max_body_bytes`。超过上限时：`fail_closed` 的 filter 返回 413，fail-open 的跳过。**不会截断后传给你** —— 一个基于半截数据做判断的安全 filter，可能得出和后端处理完整数据不同的结论。

**响应 body 是另一个开关。** `post_handler` 和 `on_error` 阶段能拿到 `req.ResponseStatus` 和 `req.ResponseHeader`，但 `req.ResponseBody` 只在声明了 `needs_response_body: true` 时才有内容 —— 否则它是一个空切片，而不是一个错误。想改写或检查响应体的 filter 必须显式声明：

```yaml
filters:
  - name: inject-banner
    phase: post_handler
    match: { paths: ["/api/plugins/notes/**"] }
    needs_response_body: true
    max_body_bytes: 262144
```

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

放到整条请求上看（本机实测，含 HTTP 收发、管道、跨进程往返）：

| 场景 | 每请求 | 约合 |
|---|---|---|
| Core 自己的路由（对照） | 52 µs | 19,000 req/s |
| 插件路由，无 filter | 95 µs | 10,500 req/s |
| + 1 个 filter | 141 µs | 7,000 req/s |
| + 3 个 filter | 242 µs | 4,100 req/s |
| + 5 个 filter | 337 µs | 3,000 req/s |

每多一个命中的 filter 大约 +48 µs，其中约 37 µs 是跨进程往返本身。这个量级决定了一件事：**filter 要按路径精确订阅，而不是靠代码里 `if` 掉不关心的请求** —— 后者已经付过了跨进程的钱。

把跨进程那一跳单独拆出来看（同样是本机实测）：

| | 每次调用 |
|---|---|
| 一次 filter 调用（纯 RPC，无 HTTP） | 37 µs |
| 一次 backend 调用（纯 RPC） | 43 µs |
| 载荷 0 字节 | 43 µs |
| 载荷 1 KB | 43 µs |
| 载荷 16 KB | 107 µs |
| 载荷 256 KB | 944 µs |

**到 1KB 为止，成本和载荷大小无关** —— 那 37µs 基本全是跨进程往返本身，不是数据搬运。两个推论：小 body 不值得省，而大 body 很贵（16KB 就翻了一倍半），所以真正的大块内容应该走文件能力而不是塞进插件响应。

复现：`go test ./tests/ -bench=E2E -benchtime=800x -run=XXX`

---

## 多副本

`runtime.replicas` 大于 1 时，Core 用 nginx 的平滑加权轮询在副本间分流，`weight` 决定比例。实测 3 个等权重副本处理 300 个请求是精确的 100/100/100，权重 1:4 处理 250 个请求是精确的 50/200 —— 这个算法是确定性的，不是概率性的。

副本是**独立进程，不共享内存**。任何存在进程内的状态在多副本下都会各算各的：计数器、令牌桶、缓存、去重表。需要共享就放进 `sdk.Cache` 或文档存储，代价是每次访问多一次跨进程调用。

一个副本进程死掉时，调度器会跳过它，存量流量由其余副本承接。死亡瞬间正在那个进程里处理的请求会失败 —— 这一点没有任何架构能避免，因为那些工作就在刚刚消失的内存里。真正重要的是失败**只限于那一刻**：实测在持续压测下杀掉三副本之一，15616 个请求中 4 个失败，全部发生在死亡瞬间，之后为零。

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

### 崩溃、重启与隔离

插件进程意外退出时，Core 会按指数退避重启它（1s、2s、4s……封顶 60s）。这对偶发崩溃是对的做法。

但反复崩溃不会被无限重启。窗口期内崩溃次数超过阈值后，插件进入**隔离**（quarantine）：路由被摘除、进程被杀、不再重启，控制台上标记为已隔离并记录时间。开机就崩的插件否则会永远重启下去，烧 CPU、刷日志，还把真正的故障淹没在噪声里。

隔离是终态，需要管理员显式处理 —— 在控制台重新「启用」即清除隔离记录和崩溃计数，重新开始。

对插件作者的含义：**启动阶段的失败要尽快、明确地失败**。

`Configure` 是 SDK 内部的握手，不是你要实现的钩子 —— `sdk.Config` 里没有这一项。你能控制的是启动时会不会 panic：SDK 在 `Configure` 里做的事失败了，这次启用会直接失败并保留旧版本，比启动后再崩溃干净得多。所以初始化里该炸的东西要在 `sdk.Serve` 之前或者第一次 `OnConfigChanged` 里炸掉，不要拖到第一个请求。

`sdk.Config` 的全部字段就这些：`Handler`、`Filters`、`Jobs`、`OnConfigChanged`、`OnShutdown`、`MaxMessageBytes`。

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
