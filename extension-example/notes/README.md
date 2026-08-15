# 笔记插件示例

一个完整的插件：自己的 HTTP API、自己的表、自己的菜单、自己的定时任务，外加两个 filter。它是「插件作为目的地」的样子 —— 用户会点开它、在它的页面上做事。

对照另外两个：[`ratelimit`](../ratelimit) 是纯 filter，不拥有任何路由；[`audit`](../audit) 只在别人的请求结束后记一笔。三个放在一起才是完整的插件模型。

```
CGO_ENABLED=0 go build -o bin/plugin .
```

## 值得看的几处

**查询用游标，不用 offset。**

```go
q := sdk.DB.Where("notes").SortDesc("created").Limit(50)
if after := r.URL.Query().Get("after"); after != "" {
    q = q.After(after)
}
next, err := q.All(ctx, &notes)
```

`All` 返回下一页的游标，原样传回 `After` 就是下一页。用 offset 的代价是数据库要逐行跳过并丢弃，翻得越深越慢，而且期间有行插入或删除会导致重复或遗漏 —— 那不是「稍微不准」，是用户会看到同一条笔记出现两次。

**时间存成 RFC3339 字符串，不是 unix 整数。**

比较值一律是字符串，所以字典序必须等于时间序才能用 `Gt` / `Lt` 筛时间范围。`"2026-08-16T03:17:00Z"` 可以；`"999"` 和 `"1000"` 会告诉你 999 更大。

**定时任务只做决定，把重活交给队列。**

```go
_, _, err = sdk.Queue.Publish(ctx, "summaries", summary,
    sdk.WithDedupKey(fmt.Sprintf("summary-%d", job.Scheduled)))
```

job 的 handler 在跑的时候占着这个插件的一个请求名额，排空要等它 —— 所以一个跑十分钟的夜间汇总会让那十分钟里的升级卡住。把结果投进队列，job 本身就是毫秒级的。

去重键用的是 `job.Scheduled` 而不是 `time.Now()`：那是这次运行**本该**发生的时刻。Core 忙、插件刚重启、任务排队都会让实际执行晚于计划，而按「本该的时刻」去重，重跑同一个占位的任务不会产生第二份汇总。

**两个 filter，两种用途。**

`pre_route` 那个会拦请求（它可以短路），`log` 那个不会 —— 它在响应发出之后才跑，返回值被忽略。后者是审计和埋点该待的地方，因为它不可能拖慢任何东西。

代价是它也不可能保证记下来：插件崩了、熔断开了、正在排空的那段时间，请求照常成功而记录静默丢失。不能丢的东西要在 `log` 阶段里投进持久队列。

## 它需要什么

```yaml
permissions: [db, queue, events, cron]
```

`db` 建表存笔记，`queue` 投递汇总，`cron` 跑定时任务，`events` 广播笔记创建。审核这个插件的人从这四行就能知道它能碰什么 —— 它要不到文件写入，也要不到出站网络。
