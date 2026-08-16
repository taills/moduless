# digest — 外部快照

在计划时间抓取一个外部 URL，**内容变了才**存一份，并给管理员一个短时下载链接。

它是第八个示例，存在的理由是另外七个都不碰的三项能力：**出站 HTTP、写文件、发下载链接**。

## 三项能力，一个共同的形状

插件从不自己碰网络或对象存储，它请求 Core，规则在 Core 那一侧执行。

| 能力 | 插件做什么 | Core 做什么 |
|---|---|---|
| `http:egress` | `sdk.HTTP.Get(ctx, url)` | 校验域名在 `egress_allow` 里，**再校验解析出的 IP**，不跟随重定向 |
| `files:write` | `sdk.Files.Put(...)` | 写入对象存储，返回 file id |
| `files:read` | `sdk.Files.DownloadURL(...)` | 签发短时令牌，浏览器直接来取 |

`files:read` 和 `files:write` 分开是有原因的，这个插件正好说明：它**只写不读** —— 存快照、发链接，一个字节都不读回来。一个能给自己没写过的文件发链接的插件，就是一条枚举别人文件的路。

## 这里最值得抄的东西：接缝

Core 拒绝插件出站到 loopback 和私有地址。这是对的 —— 正是它挡住「白名单里的域名解析到 `169.254.169.254`」—— 但它意味着**作者无法把 `sdk.HTTP` 指向一个测试服务器**：没有任何地址是测试能监听、而 Core 又肯拨的。其他能力有同样的问题但原因不同：`sdk.Files` 要跟 Core 说话，而 `go test` 下没有 Core。

所以能力都放在接缝后面。每个接缝一到两个方法，而且**SDK 客户端直接满足它们，不需要任何适配器**：

```go
type fetcher interface {
    Get(ctx context.Context, url string) (*http.Response, error)
}

var _ fetcher = (*sdk.HTTPClient)(nil)   // 编译得过
```

能这么干，是因为 SDK 用的是 `*http.Response`、`io.Reader` 和普通字符串，而不是自己发明的类型。`main.go` 接真的，`main_test.go` 接一个 `httptest.Server` 和两个记账用的假实现 —— 整个 job 就此可测，不需要 Core、数据库或对象存储。

`main_test.go` 末尾那三行 `var _ = ...` 就是把这个说法交给编译器去核对，而不是让读者相信注释。

**唯一没有天然接缝的**是 `sdk.DB.Where(...)` —— 它返回一个具体的链式构造器，假造它等于重写整个构造器。所以 `list` handler 没有单测，而 job 有七个。这是宿主接口里目前唯一的例外。

## 逻辑本身

只有一件事值得写代码：**内容没变就什么都不存**。

一个每次运行都写文件的定时任务，会永远累积一模一样的副本，而最先注意到的通常是存储账单。所以它先算 sha256，跟上次的比：

- 一样 → 记一条日志就返回，不写文件
- 不一样 → 存文件、索引这次快照、最后更新「上次的哈希」

顺序是有意的。哈希放在最后写，所以两步之间崩溃留下的是「一次会被重做的快照」，而不是「一个指向没人存过的内容的标记」。

「上次的哈希」存在文档库里而不是结构体字段里，所以**重启不会重新归档** —— `TestARestartDoesNotReArchive` 钉的就是这一条。

还有一个容易写反的地方：时间戳用 `job.Scheduled` 而不是 `time.Now()`。它是这次运行**本该**发生的那一刻，Core 忙或者插件刚重启都不会让它漂移。把它换成 `time.Now()`，`TestTheFirstRunArchivesWhatItFetched` 会失败 —— 这是实测过的，不是推断。

## 运行

```bash
CGO_ENABLED=0 go build -o bin/digest .
```

manifest 里 `egress_allow` 只写了 `api.github.com`，`source_url` 默认指向 `https://api.github.com/meta`。改 `source_url` 指向别处之前，得先把那个域名加进 `egress_allow` 并重新审核 —— 否则调用返回 `PermissionDenied`，而这是永久错误，重试没有意义。

四种出站失败的状态码是分开的，因为处理方式完全不同：

| 状态码 | 含义 | 该怎么办 |
|---|---|---|
| `PermissionDenied` | 域名不在 `egress_allow` | 改 manifest，重新审核 |
| `ResourceExhausted` | 超出速率 | 等一下 |
| `InvalidArgument` | URL 拼错了 | 是你的 bug |
| `Unavailable` | 对端连不上 | 可以重试 |
