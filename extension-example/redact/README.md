# 响应脱敏插件示例

唯一一个**改写别的插件说出去的话**的示例。其他五个要么决定请求走不走（[`ratelimit`](../ratelimit)、[`apikey`](../apikey)），要么看着它过去（[`audit`](../audit)），要么提供自己的东西（[`notes`](../notes)、[`inventory`](../inventory)）。

```
CGO_ENABLED=0 go build -o bin/plugin .
```

## 它演示的机制：post_handler 里的 Stop

**`Mutate()` 改不了响应体。**它能改请求头、响应头、路径、身份、context —— 就是没有响应体。照着 mutation 的 API 读，会得出「改不了」的结论。

改法是在 `post_handler` 里 **`Stop()`**：这个阶段后端已经答完了，没有东西可以「拒绝」，所以短路的语义变成了**替换那个答案**。

```go
return sdk.Stop(req.ResponseStatus, cleaned), nil
```

同一个 `Stop`，在 `pre_route` 里是「拦下这个请求」，在 `post_handler` 里是「换掉这个响应」—— **阶段决定了它的含义**。

## 两个坑

**替换会丢掉后端的响应头。**Core 只写短路响应自己的头。所以最自然的写法 —— `return sdk.Stop(status, cleaned)` —— 会把它碰过的每个响应的 `Content-Type` 抹掉。症状是浏览器把 JSON 当纯文本渲染，而那个现象指向的位置离这个插件十万八千里。这个示例把头逐个抄过去：

```go
res := sdk.Stop(req.ResponseStatus, out)
for key, values := range req.ResponseHeader {
    for _, v := range values {
        res = res.WithHeader(key, v)
    }
}
```

**没改动的时候要 `Continue`，不要 `Stop`。**返回 `Stop` 既不免费也不无害：它要付上面那份头拷贝，而且会让系统里每个响应都经过这个插件对「响应长什么样」的理解 —— JSON 重新编码会打乱键序、丢掉格式。测试里专门有一条断言：没配置任何字段时，响应必须**逐字节**原样通过。

## 它的权限是空的

```yaml
permissions: []
```

这个插件看得见系统里每一个响应体，却**存不下、发不出、写不了盘** —— 没有 `db`、`queue`、`files`、`http:egress`。

审核它的时候，问题不是「它能拿这些数据做什么」，而是**它能看见这些数据本身**。审的人该读的是它记了什么日志，不是它会不会外传 —— 因为它没有外传的能力。

## 为什么只挂在两个路径上

```yaml
match:
  paths:
    - /api/plugins/notes/**
    - /api/plugins/inventory/**
```

`needs_response_body: true` 的 post_handler filter 会把响应体跨进程搬两趟。实测成本：16KB 的 body 让一次 RPC 翻倍，256KB 是 944µs。挂在 `/**` 上等于给系统里每个响应都加上这笔钱。

点名那些真的会返回个人数据的路由。

## fail-open 是对的

这个 filter 挡不住任何本该被拒的东西 —— 响应早就产生了。它坏掉的后果是响应未经脱敏发出去，等于没装它。而 `fail_closed: true` 会把这个插件的一次抖动变成全站 503。

对比 [`apikey`](../apikey)：那个是 fail-closed 的，因为它坏掉的后果是**每个请求都不经鉴权通过**。界线是「这个 filter 宕掉，会不会让本该被拒的东西过去」。
