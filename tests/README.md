# 测试说明

这个目录里的测试会 fork 真实的插件进程、连真实的 PostgreSQL 和对象存储。它们不是单元测试的补充，而是唯一能验证「跨进程边界之后行为是否还成立」的地方 —— 这套系统里最贵的几个 bug 都只在那条边界上出现。

## 跑起来

```bash
go test ./...                                  # 不需要外部依赖的部分
```

需要基础设施的测试会 `t.Skip` 而不是失败。跑全量：

```bash
# PostgreSQL
docker run -d --name moduless-test-db -p 15433:5432 \
  -e POSTGRES_USER=moduless -e POSTGRES_PASSWORD=moduless \
  -e POSTGRES_DB=moduless_test postgres:18-alpine

# S3 兼容对象存储（bucket 由测试自己创建）
docker run -d --name moduless-test-s3 -p 19000:9000 \
  -e MINIO_ROOT_USER=moduless -e MINIO_ROOT_PASSWORD=moduless123 \
  minio/minio:latest server /data

TEST_DATABASE_URL='postgres://moduless:moduless@localhost:15433/moduless_test?sslmode=disable' \
TEST_S3_ENDPOINT='http://localhost:19000' \
  go test ./... -count=1
```

**还要在 Linux 上跑一遍。** 开发机是 macOS，而两处行为不同且都要紧：写入正在执行的二进制在 Linux 上是 `ETXTBSY`，`Pdeathsig` 只在 Linux 存在。

```bash
docker run --rm -v "$(pwd)":/src -w /src -v "$HOME/go/pkg/mod":/go/pkg/mod \
  -e CGO_ENABLED=0 golang:1.25-alpine go test ./... -count=1
```

## 写测试时的一条规矩

**断言理由，不要只断言结果。**

这套件里反复出现同一种失败：测试通过了，但不是因为它声称的原因。

- 一个重定向测试断言「内网服务器没被访问」—— 它确实没被访问，但因为第一跳就被环回地址检查拦了，重定向逻辑压根没执行
- 一个元数据 SSRF 测试用了域名 —— 在非云环境里那只是 DNS 解析失败，五秒超时看起来和「被拒绝」一模一样
- 一个部署测试断言「不是 200」—— 拿到的是 `PermissionDenied`，而它名字里说的是后端缺失
- 最早一个叫 `TestE2EPermissionDenied` 的测试，调用了一个不需要任何权限的路径然后断言 200 —— 权限门开着还是关着它都通过

共同点是只检查了结果。**一个因为错误理由而正确的系统，离不正确只差一次环境变更。** 所以：拒绝要断言拒绝的**理由**，成功要断言是**哪条路径**成功的。

同理，单向测试不算测试。验证了「无权限的人看不到」，还要验证「有权限的人看得到」—— 否则一个把所有东西都拒绝的实现也能通过。

## 持续负载

```bash
SOAK=5m go test ./tests/ -run TestSoak -v
```

默认跳过 —— 它要跑几分钟。基准测试测的是吞吐，这个测的是**跑一阵子之后还对不对**：goroutine 有没有攒、堆有没有涨、在途计数最后有没有归零、延迟有没有随时间恶化。

最近一次 5 分钟（本机）：

```
383 万请求   12,774 req/s   0 失败
堆           全程 4.0MB，空闲时 4.0MB
goroutine    44–48
延迟 第1/3   p50=275µs  p99=725µs
     第2/3   p50=259µs  p99=684µs
     第3/3   p50=284µs  p99=956µs
```

延迟按运行时段分成三段统计，而不是只给一个全局 p50。原因是：单个数字分不出「系统在退化」和「这台笔记本在降频」—— 两者都表现为一个更差的数字。分段至少能说明变化是不是**渐进**发生的，那是决定要不要继续深挖的依据。实测中确实遇到过一次 p50 翻倍的运行，分段显示是全程均匀偏慢（当时同机在跑 docker 构建），而不是逐段恶化。

写这个测试时踩到的两个坑，都属于「测量工具影响被测对象」：

- 第一版记录**每个**请求的延迟。一秒一万五千个请求，那个 slice 本身就是几 MB —— 我测到的「堆泄漏」是测试自己。现在每一百个采样一次。
- 在途计数不能在负载停止的瞬间读。log 阶段在响应之后异步执行，最后几个请求还没结束；要断言的是它**最终**归零。

## 观测不要有副作用

`Breaker.Allow()` 在半开状态下会消耗掉那一个探针名额。用它来「查看熔断器状态」不是查看 —— 它改变了被查看的东西，而且一个用它轮询的测试会把自己在等的那个探针吃掉。`Breaker.Open()` 才是无副作用的那个，代码注释里就是这么写的。

这类陷阱不止一处：任何 `CompareAndSwap`、任何消费型的读取、任何带租约的 claim，拿来做断言都会改变结论。写测试时先问一句：这个观测本身会不会改变结果。

## 变异测试

判断「测试是否真的在守护某个保护」，最直接的办法是把那个保护弄坏，看有没有测试失败。没有测试失败的保护，就是没有被守护的保护。

做法：在保护函数开头插一句 `if true { return <放行> } // MUTATION`，跑全套，记下失败的测试，然后还原。

最近一次的结果：

| 保护 | 失败的测试数 |
|---|---|
| 权限门（`hostsvc.require`） | 8 |
| SSRF 地址守卫（`blockedIP`） | 4 |
| 文件所有权（`checkOwnership`） | 3 |
| 二进制校验（`SecureConfig`） | 2 |
| 状态码归一化（`normalizeStatus`） | 2 |
| 路径清理（`cleanSubPath`） | 1 |
| 身份改写阶段门（`identityMutationAllowed`） | 1 |
| 环境变量隔离（`SkipHostEnv`） | 1 |

文件所有权那一行原本是 **1**，而且那唯一的测试需要数据库和 S3 —— 基础设施缺席时它会被 skip，保护就完全裸奔。补了删除和元数据两条路径后变成 3。

只有 1 个测试守着的那几项值得留意，不过它们的测试都不依赖外部基础设施，不会被 skip。

改动任何安全相关的代码后，值得对它做一次这个动作。


## 测量（非默认执行）

```bash
# 事务上限对吞吐的影响，约一分钟
MEASURE=1 TEST_DATABASE_URL=... go test ./tests/ -run TestTransactionCeilingThroughput -v

# 只重测某几个上限
MEASURE=1 CEILINGS="4 8" TEST_DATABASE_URL=... go test ./tests/ -run TestTransactionCeilingThroughput -v
```

**基准会污染自己。**这个测量最初复用同一个集合，于是每轮往同一行打十几万次更新，留下同样多的死元组；在 autovacuum 追上之前，下一轮量到的是膨胀而不是上限。相隔一小时的两次扫描，绝对吞吐差了 3 倍，而形状完全一致。现在每轮用自己的集合并在结束时删掉。

结论只看形状，不看绝对值 —— 后者取决于这台机器此刻在干什么。
