# API 密钥插件示例

唯一一个**决定调用者是谁**的示例。其他三个 filter 要么观察（[`audit`](../audit)）要么拒绝（[`ratelimit`](../ratelimit)），这个告诉 Core 一个身份，而下游所有东西 —— 别的插件的 filter、它们的 handler、审计日志 —— 都会相信它。

```
CGO_ENABLED=0 go build -o bin/plugin .
```

## 它测出来的东西

**`sdk.Cache` 不是本地缓存，是一次 RPC。**

缓存在 Core 里，所以「缓存命中」仍然是一次跨进程往返。实测每次调用：

| | 每次调用 |
|---|---|
| 无凭据（最便宜的路径） | 39 µs |
| 只用 `sdk.Cache` | **124 µs** |
| 进程内缓存挡在前面 | **43 µs** |

差出来的 80µs 不是这个插件的延迟，**是所有人的**：authenticate filter 订阅了 `/**`，所以每一个请求都要付，包括那些属于完全无关插件的请求。

代价是撤销延迟：本进程会继续用一个已撤销的密钥，直到自己的条目过期，而别的副本清不掉它。所以 `local_ttl` 默认 5 秒（Core 侧那层是 60 秒），并且是单独可配的 —— 它配的其实是**一个被撤销的密钥最长还能用多久**。

## 三处值得看的

**authenticate 只回答「你是谁」，不回答「你能做什么」。**

密钥缺失、错误、已撤销，这个 filter 都返回 `Continue` 而不是 `Stop` —— 调用者只是保持匿名。拒绝是 `authorize` 阶段的事，因为很多路由本来就是公开的，而 authenticate filter 不知道是哪些。

分成两个阶段还有一个更实在的理由：Core 只在 authenticate 和 authorize 阶段、且只对持有 `filter:authenticate` 的插件采纳 `SetIdentity`。于是「你是谁」和「你能做什么」可以分给不同的插件，一个插件可以被信任做第二件事而不被信任做第一件。

**两个 filter 都是 `fail_closed: true`。**

默认是 fail-open，对观察型 filter 是对的，对这里是灾难：插件崩了、熔断开了、超时了，每个请求都会**未经鉴权**通过。一个只在插件健康时才生效的鉴权器不是鉴权器。

**存的是哈希，不是密钥。**

明文只在创建时返回一次。用 SHA-256 而不是 bcrypt 是刻意的：这段代码在每个请求上跑，一个故意很慢的哈希放在 authenticate filter 里，等于给了任何人一个发错密钥就能触发的拒绝服务。这个取舍成立的前提是密钥是 256 位随机数而不是密码 —— 没有字典可以拿来跑。

## 权限

```yaml
permissions:
  - db
  - cache
  - filter:authenticate
```

`filter:authenticate` 是这张表里唯一会改变**别的插件看到什么**的权限。没有它，Core 会丢弃这个插件返回的任何 `SetIdentity`，并记一行日志说它试过。审核这份 manifest 的人批准的是「这个插件决定调用者是谁」，而不是「这个插件读一个 header」。

这条不是纸面上的：`tests/apikey_test.go` 用同一个插件二进制、只改授予的权限跑两遍，确认没有权限时身份不会被采纳 —— 检查在 Core 那一侧，插件无法选择不被检查。

## 它修好的东西

写这个示例之前，它描述的场景**在生产里根本不成立**：Core 解析 session 失败就直接 401，而那发生在 authenticate 阶段之前。一个带着 API key 但没有 session cookie 的请求，会在那个懂它凭据的插件被问到之前就被拒掉。`filter:authenticate` 这套机制存在、被文档描述、有权限门禁保护，但没有任何请求能走到它。

现在 Core 把那个 401 推迟到 authenticate 和 authorize 之后 —— 问题还是同一个问题，只是换了个时机问，好让该回答的人有机会回答。没装 authenticate filter 的部署行为完全不变。

代价写在文档里：插件路由仍然不能是公开的，因为那个 401 是无条件的。

## 它不能自举

签发密钥要 admin 角色，而在一个开着 session 鉴权的 Core 上，那个 admin 是一个**有登录会话的人**。所以**第一把密钥必须由人在控制台上签发**，这个插件没法给自己发第一把。

这不是缺陷，是信任链本来的样子：如果这个插件能给自己签发第一把密钥，那么任何能触达它的人就都能。代价是部署时多一步带外操作 —— 登录控制台、发一把、交给需要它的服务。

（`tests/six_plugins_test.go` 里那个组合测试因此是直接往集合里写一行，而不是调这个接口 —— 它的网关没有 session 存储。这正是真实部署里那一步的形状。）

## 菜单上的 roles 不保护路由

```go
if !sdk.User(r.Context()).HasRole("admin") {
    http.Error(w, "forbidden", http.StatusForbidden)
    return
}
```

`menus[].roles: [admin]` 只决定菜单项显示给谁。任何知道 URL 的人都能直接 POST `/api/plugins/apikey/keys` 给自己发一个 admin 密钥。这个仓库已经因为漏掉这一步交付过一次所有人可读的审计日志。
