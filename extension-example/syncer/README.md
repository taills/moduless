# syncer — 用锁在自己的副本之间串行化

其余五个示例都没有取过锁，因为它们做的事不需要。这个需要：**两个副本消费同一个队列**，而同一个账户的两条消息可能同时落到两个进程上。它模拟的外部系统（多数都如此）不接受对同一账户的并发写，所以这段工作必须串行 —— 而且必须**跨进程**串行，进程内的 `sync.Mutex` 在这里没有意义。

```bash
CGO_ENABLED=0 go build -o syncer/bin/plugin ./extension-example/syncer
cp extension-example/syncer/manifest.yaml syncer/
PLUGIN_DIR=$(pwd) go run ./core

curl -XPOST localhost/api/plugins/syncer/sync -d '{"account":"acct-1"}'
```

## 先说什么**不**需要锁

伸手去拿锁最常见的三种场合，其实都不需要：

| 场合 | 为什么不需要 |
|---|---|
| **cron 任务** | Core 的调度器对每次触发只挑**一个副本**（`core/pluginhost/scheduler.go`），任务本来就只跑一次，副本数是多少都一样 |
| **进程内的状态** | `sync.Mutex` 免费且正确。跨进程的锁要走一次 RPC |
| **两个插件之间协调** | 锁名按插件 key 分命名空间，这个插件的 `sync` 和别的插件的 `sync` 是**两把不同的锁** —— 故意如此，否则一个插件选了个常见词就能拖住所有人 |

剩下的才是这里的情形：**同一插件的多个副本，够到了同一个外部对象。**

## 锁名就是互斥的粒度

```go
lease, ok, err := sdk.Locks.Acquire(ctx, "account:"+account, ttl, lockWait)
```

按账户命名，而不是一把全局的 `"sync"`。同时同步**不同**账户的两个副本应该都能跑下去；用一把全局锁虽然也“正确”，但第二个副本就白设了。

拿不到锁时返回错误让消息**退回队列**（带退避重试），而不是丢弃 —— 这份工作仍然要做，只是现在不该由这个副本做。

## 「忙」不能当成「失败」

拿不到锁时最自然的写法是返回错误让消息退回队列。**这会静默地丢工作。**

队列的重试计数 `attempts` 是在**消息被认领时** +1 的，不是在失败时。所以一条反复被「拿不到锁的副本」取走的消息，会一路烧完自己的预算（默认 5 次）然后进死信 —— **它一次都没失败过**。实测：一个热账户 6 条消息、两个副本、零等待，**3 秒内 4 条进了死信**，死信原因还写着「account is being synced by another replica」，读起来像是对这份工作的诊断，其实是记账假象。

所以这里的做法是：**确认掉这次投递，把工作重新发布一条**（带一秒延迟）。新消息带新的预算 —— 这是对的，忙不是失败。payload 里的 `Requeues` 跟着走，超过上限（20）才真的失败，让确实卡死的账户能出现在死信里而不是永远打转。

```go
if !ok {
    return handleBusy(ctx, j)   // ack 掉这次，另发一条
}
```

顺带：`lock_wait_seconds` 设得比一次同步的耗时长，这类搬运基本不会发生 —— 等锁比放回去便宜。上面那组数字是把等待设成 0 逼出来的最坏情况。

回归测试：`tests/contention_test.go`。

## 长活要续租

租约会过期。这是它的作用：持有者进程崩了，别的副本得能接手，而不是这个账户永远被锁死。代价是**干得比租约久，锁会在你还在用的时候悄悄放开**。

所以工作放在后台，主循环按 `ttl/3` 续租：

```go
case <-renew.C:
    held, err := lease.Renew(ctx, ttl)
    if err != nil || !held {
        return errLostLock   // 停下，别干完
    }
```

`Renew` 返回 false 意味着**租约已经不在了，别人可能已经在这个账户上了**。这时正确的动作是**停下**而不是干完 —— 干完才是双写。

## 后台工作放在 `OnReady`

队列消费者不能在 `main()` 里启动：`sdk.Queue` 要等 Core 递过反向连接才有值，而那发生在 `sdk.Serve` **之后**，在 `main()` 里拿到的是 nil。也不能放在 `OnConfigChanged` —— 它每次配置变更都会再触发一次，每改一次配置就会多出一个消费者。

```go
sdk.Serve(sdk.Config{
    OnReady: func(ctx context.Context) {
        err := sdk.Queue.Consume(ctx, "accounts", handle)
        ...
    },
})
```

`OnReady` 只跑一次，在初始配置应用**之后**（所以能读到管理员配的值），拿到的 ctx 会在 Core 要求排空时取消 —— 阻塞中的 `Consume` 因此是自己返回，而不是被杀掉。

## 副本真的会分担队列

写这个示例时，它的前提**不成立**：加副本毫无收益（1 副本 2.04s，2 副本 2.03s），一个副本吸走全部消息、另一个空转。

根因在 Core：服务端 `Consume` 把消息 `stream.Send` 出去就返回、不等 Ack，所以 **`prefetch` 限制的是单次认领的批量，不是未确认的在途数**。一个消费者能在毫秒内认领整个积压。第二个后果更隐蔽 —— **可见性超时是在消息躺在流缓冲里时走的**，积压够大时，消息会在插件还没开始处理前就被判超时重投。

现在按未确认数限流，实测 **1 副本 2.05s → 2 副本 1.11s**。回归测试在 `tests/syncer_test.go`：`TestPrefetchBoundsUnacknowledgedMessages`（去掉限流会变成「prefetch=1 却同时认领 4 条」）和 `TestASecondReplicaSharesTheQueue`。

顺带一提 **`prefetch` 的含义**：它是「我最多同时握住几条未确认的消息」。SDK 的 `Consume` 目前固定发 1，也就是**一次一条、处理完再要下一条**。这对本示例是对的（每条消息要占住一个账户锁一段时间），但它意味着单个副本不会并发处理消息 —— 要更高吞吐就加副本，而不是指望一个进程内部并行。

## 这把锁保护的边界

它是 **Core 进程内**的一张表，按插件 key 分命名空间。够用是因为 Core 是单实例、所有插件副本都是它的子进程 —— 也就是这个框架**唯一存在的拓扑**。

它不保护的：两个 Core、或者这个插件之外的任何东西。真要那种，锁得挪到 Core 之外（Postgres 咨询锁、Redis、etcd），而接口形状不用变。

跨副本互斥本身有实测：`tests/lock_replica_test.go`。那条测试特意用**共享**一份 host 能力的副本 —— 测试助手 `launchReplica` 给每个副本各建一份，用它写这个测试会看到两个副本都拿到锁，而错在脚手架。
