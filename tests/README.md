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


## 热路径回归

墙钟基准是延迟的诚实度量，但在笔记本上挡不住回归 —— filter 深度基准的轮间方差比值得抓的变化还大，而且**一组基准的第一次运行必然偏高**（实测 filters_1 首跑 176µs，随后三次都是 143–147µs）。

所以护栏用的是分配次数，它不随 CPU 负载变化：`core/pipeline` 的 `TestFilterCallAllocationBudget` 断言单个 filter 的管道开销和每多一个 filter 的边际开销都在预算内。

当前实测：**每个 filter 5 次分配**。对照端到端基准的每 filter 约 129 次 —— 另外那 ~124 次是 gRPC 跨进程往返，不是管道。深管道贵在进程边界，这个测试的作用就是让它一直是这样。

这个测试第一版守不住任何东西：它用的是空 header map，而它要抓的回归（每个 filter 重复转换 header 而不是转换一次）在没有 header 时不分配任何东西。现在用的是真实请求会带的那几个头，把 header 缓存关掉能让它从 5 涨到 14 并失败。

一次跨 40 轮改动之后的比对，与 `docs/plugin-development.md` 里记录的数字：Core 自身路由 52→41µs（更快），纯 RPC 37/43→38/43（不变），filter 路径全部落在噪声内。没有回归。


## 生命周期顺序

`tests/lifecycle_test.go` 是唯一一处断言**一次真实请求经过真实插件进程时，阶段实际发生的顺序**。在它之前，每个阶段都被单独测过，顺序也在 pipeline 层用 fake 测过 —— 而上一个 bug 恰好活在这两者之间：401 和 authenticate 各自都对，只是顺序反了，于是那个阶段永远不可达。**两半都正确、顺序错误的 bug，任何单元测试都抓不到。**

序列不是从副作用推断的，是从插件里读回来的：fixture 记录每个 trace 被调用过哪些阶段，测试再问它。断言的是发生了什么，而不是应该发生什么。

覆盖：完整顺序、`on_error` 成功时不触发 / 后端死掉时触发、被短路的请求仍然跑 `log`、跨插件的 `order` 决定谁先跑（用「低 order 的拒绝，高 order 的不该被调到」来观测，因为两个 Continue 的先后从外部看不出来）。


## 运维路径要用真实流量测

`tests/integration_test.go` 里的升级/停用测试打的是 `Manager.Upgrade` 和 `Manager.Disable` —— 运维在控制台上点「重载」「停用」实际触发的那条路 —— 并且**流量对准正在被改动的那个插件**。

在它之前测过的是：registry 层的蓝绿切换（`reg.Swap`）带负载，以及 manager 升级但流量打在**别的**插件上。两者都没有覆盖运维真正制造的那个场景。第一次跑就抓到了：停用一个正在服务的插件，5801 个请求收到 502。


## 只读文档的作者实验

派一个 agent，**只允许读 `docs/plugin-development.md`** —— 不给 SDK 源码、不给示例、不给测试 —— 让它写一个真实插件，记下每一处「文档没说、只能猜」，然后把它的猜测拿去编译。编译器给出的是不容争辩的度量。

两轮的对照：

| 上一轮补进文档的 | 作者 1（补之前） | 作者 2（补之后） |
|---|---|---|
| `SetIdentity` 签名 | 编译失败（猜成字符串） | 正确 |
| `OnShutdown` | 不知道存在 | 用了 |
| `Consume` 阻塞 | 低信心猜测 | 正确 |
| 事务里的 `PutIfVersion` | 没尝试 | 用了 |

作者 2 唯一的失败（3 个错误、1 个根因）是**上一轮的文档修复自己制造的**：`QueueMessage` 的字段只列了名字没给类型，它把 `ID` 当成了字符串。

所以现在文档里给的是结构体声明，并且有 `TestGuideStructsMatchTheSDK` 守着 —— 文档展示的每个字段必须在 SDK 上存在且类型一致。它不要求文档展示全部字段（只展示有意思的那半是合理的），只要求**展示出来的是真的**。

用它变异验证过：把 `ID` 写成 `string`、或加一个不存在的字段，都会失败。


## 静态链接那条规则，实测出来的样子

`CGO_ENABLED=0` 那条规则的失败信息，之前三处文档都写成 `exec format error`。实测不是。

复现（需要 Docker）：

```bash
# 在 glibc 里造一个动态链接的插件
docker run --rm -v "$PWD:/src" -w /src golang:1.25 \
  sh -c 'CGO_ENABLED=1 go build -o /src/.dyn ./tests/fixtures/echoplugin'

# 在 musl 里执行它
docker run --rm -v "$PWD:/src" alpine /src/.dyn
# sh: /src/.dyn: not found        ← 文件就在那儿
```

Go 的 `exec.Command` 看到的是 `fork/exec /src/.dyn: no such file or directory`。

内核返回 ENOENT 是因为**动态链接器**不存在（`/lib/ld-linux-*.so`），不是那个二进制 —— 但错误指着二进制说它不存在。**这不是「不指向原因」，是指向了错误的方向**：看到这句话的人会去查路径、查挂载、查权限，而问题在编译参数上。

`exec format error` 是 ENOEXEC，架构不符时才出现（arm64 上跑 amd64）。让人去找它，就是让人永远找不到。

这个实验没有做成自动化测试：它需要 Docker 加一次 glibc 构建，代价远超收益。留下的是可复现的命令和确切的输出。
