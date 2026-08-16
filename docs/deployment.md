# 部署指南

Core 是**单实例**设计。韧性来自进程守护重启 Core、以及 Core 自己重启插件，而不是来自跑多份 Core —— 进程内的缓存、锁和会话存储都建立在这个前提上。

## 组成

| 组件 | 必需 | 说明 |
|---|---|---|
| Core | 是 | 唯一监听端口的进程（默认 `:80`） |
| PostgreSQL | 否 | 提供文档存储、队列、认证、审计。不配则这些能力报 Unavailable，插件仍能运行 |
| 对象存储（S3 兼容） | 否 | 文件上传下载。不配则文件能力不可用 |
| 插件 | — | 不是独立服务，是 Core 启动的子进程 |

插件**不需要**在编排层声明。Core 扫描 `PLUGIN_DIR` 并自己管理它们的生命周期。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `HTTP_ADDR` | `:80` | 监听地址 |
| `DATABASE_URL` | — | 启用数据、队列、认证、审计 |
| `PLUGIN_DIR` | `./plugins` | 插件包目录 |
| `PLUGIN_DATA_DIR` | — | 每插件私有可写目录的根 |
| `PLUGIN_LOG_LEVEL` | `warn` | 插件日志级别 |
| `HOST_FRONTEND_DIR` | `./core/frontend/dist` | 控制台构建产物 |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | `admin` / `admin123` | 首次启动播种 |
| `RUSTFS_ENDPOINT` / `RUSTFS_BUCKET` / `RUSTFS_ACCESS_KEY` / `RUSTFS_SECRET_KEY` | — | 对象存储，四者齐全才启用 |

默认管理员**只在用户表为空时**播种。一个已有用户的数据库不会再生成管理员 —— 如果忘了密码，改数据库里的 `password_hash`，或者用另一个管理员账号重置。

## 插件包的交付

一个插件包是一个目录：

```
$PLUGIN_DIR/
└── notes/
    ├── manifest.yaml
    ├── bin/plugin        # CGO_ENABLED=0 静态二进制
    └── frontend/         # 可选，微前端 dist
```

目录名必须与 `manifest.yaml` 里的 `key` 一致，否则 Core 拒绝加载并在控制台报错。

交付方式有两种，各有取舍：

**挂载卷**（compose 默认）。更新插件不需要重建 Core 镜像，适合插件迭代比 Core 快的情况。

**烘进镜像**。基于 Core 镜像做一层，把插件 `COPY` 进去。部署单元自洽、可回滚到确定的组合，适合插件与 Core 版本强绑定的情况。

### 更新一个插件必须用 mv，不能用 cp

热更新时旧版本仍在处理请求，直到切换提交。

**在 Linux 上，内核会直接拒绝这次覆盖。**`cp` 和 `>` 都会失败：

```
cp: cannot create regular file '.../bin/plugin': Text file busy
```

也就是 ETXTBSY —— 只要有进程正在执行这个文件，就不能写它。所以用 `cp` 的后果是**部署失败**，而不是进程被破坏；而且这个错误是明说的，不像另外两条那样指向别处。

（「破坏内存映像」是没有 ETXTBSY 的平台上会发生的事 —— 比如 macOS 开发机。目标平台是 Linux，那里你得到的是一次响亮的失败。）

`mv` 之所以对：它创建新 inode 再改名，旧进程继续持有旧 inode，实测替换后原进程照常运行。

```bash
# 正确：写到临时文件再 rename（新 inode，旧进程继续用旧 inode）
CGO_ENABLED=0 GOOS=linux go build -o /tmp/plugin.new ./myplugin
mv /tmp/plugin.new $PLUGIN_DIR/notes/bin/plugin

# 错误：直接覆盖
cp /tmp/plugin.new $PLUGIN_DIR/notes/bin/plugin
```

然后在控制台点「重载」，或调用：

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://core/api/system/plugins/notes/upgrade
```

Core 会先启动新版本并完成握手，成功才切换流量，然后排空旧版本。新版本起不来时，什么都不会发生 —— 旧版本继续服务，不需要回滚操作。

## Docker Compose

见仓库根目录的 `docker-compose.yml`。

一个需要注意的点：PostgreSQL 18 的官方镜像要求卷挂在 `/var/lib/postgresql`，而不是过去惯用的 `/var/lib/postgresql/data`。挂错路径镜像会拒绝启动。

## Kubernetes

Core 是单实例，所以是 `Deployment` 且 `replicas: 1`、`strategy: Recreate`（不要 RollingUpdate —— 两个 Core 同时跑会各自 fork 一套插件）。

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: moduless-core
spec:
  replicas: 1
  strategy:
    type: Recreate
  template:
    spec:
      containers:
        - name: core
          image: moduless-core:latest
          ports:
            - containerPort: 80
          env:
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef: { name: moduless, key: database-url }
            - name: PLUGIN_DIR
              value: /app/plugins
          volumeMounts:
            - name: plugins
              mountPath: /app/plugins
            - name: plugin-data
              mountPath: /app/plugin-data
          livenessProbe:
            httpGet: { path: /healthz, port: 80 }
            initialDelaySeconds: 10
          readinessProbe:
            httpGet: { path: /healthz, port: 80 }
      volumes:
        - name: plugins
          persistentVolumeClaim: { claimName: moduless-plugins }
        - name: plugin-data
          persistentVolumeClaim: { claimName: moduless-plugin-data }
```

插件进程与 Core 在同一个容器里，所以给这个 Pod 的 CPU 和内存要覆盖 Core 加上所有插件。

### 容量：一个插件要多少

实测（`MEASURE=1 go test ./tests/ -run TestPluginProcessCost -v`，macOS，一个只做 echo 的 Go 插件）：

| 插件数 | 启动耗时 | 每个 RSS | 子进程总计 | Core 自身堆增量 |
|---|---|---|---|---|
| 1 | 298 ms | 20.0 MB | 20.0 MB | 1.0 MB |
| 5 | 191 ms | 19.1 MB | 95.7 MB | 1.4 MB |
| 10 | 391 ms | 18.2 MB | 181.9 MB | 2.6 MB |
| 20 | 830 ms | 18.7 MB | 374.7 MB | 5.3 MB |

三件事可以据此规划：

**每个插件约 19 MB，且不随数量变化。**这是 Go 运行时加 gRPC 的底价 —— 一个空插件和一个有业务逻辑的插件，差的是后者自己的数据。**20 个插件约 375 MB**，再加 Core 自己。

有一处要说清楚：这个测量里 20 个进程跑的是**同一个二进制**，所以它们的代码段是共享的，RSS 会把共享页重复计入 —— 真实的增量比 19 MB 低。但真实部署里插件是**不同的**二进制，没有这份共享，所以 19 MB 反而是那种情况下诚实的边际成本。

**Core 自身每多一个插件约 0.26 MB**，可以忽略。这个架构的成本几乎全在子进程上，不在 Core 里。

**启动是线性的，每个约 39 ms。**（`TestPluginStartupIsLinear` 断言这一点：4 个和 16 个时每个都是 39 ms。）所以 20 个插件的 Core 重启后大约 0.8 秒才全部就绪，50 个约 2 秒。这个数字要和你的健康检查 `initialDelaySeconds` 对上。

## systemd

```ini
[Unit]
Description=Moduless Core
After=network.target postgresql.service

[Service]
ExecStart=/usr/local/bin/core
Environment=HTTP_ADDR=:8080
Environment=PLUGIN_DIR=/var/lib/moduless/plugins
Environment=PLUGIN_DATA_DIR=/var/lib/moduless/plugin-data
EnvironmentFile=/etc/moduless/env
Restart=always
RestartSec=3

# 插件继承这个用户的权限（IIS Filter 模型），所以不要用 root 跑
User=moduless
Group=moduless

# Core 退出时连同它 fork 的插件一起清理
KillMode=control-group

[Install]
WantedBy=multi-user.target
```

`KillMode=control-group` 很重要：Core 用 `Setpgid` 把插件放进自己的进程组，systemd 据此能一并终止它们。加上 Linux 上的 `Pdeathsig`，Core 无论正常退出还是崩溃，都不会留下孤儿插件进程。

## 备份

需要备份的：

- **PostgreSQL** —— 用户、审计、队列，以及所有插件的数据（`ext_*` 表）
- **`$PLUGIN_DIR`** —— 插件包本身。也可以从构建产物重新生成，取决于你的交付方式
- **`$PLUGIN_DATA_DIR`** —— 插件写的私有文件
- **对象存储** —— 上传的文件

不需要备份的：Core 自身无状态。

### 怎么备份

```bash
# 数据库。-Fc 是自定义格式，pg_restore 能选择性恢复单张表
docker exec <pg-容器> pg_dump -U moduless -d moduless -Fc > moduless-$(date +%F).dump

# 插件包与插件的私有文件
tar czf plugins-$(date +%F).tgz "$PLUGIN_DIR" "$PLUGIN_DATA_DIR"
```

对象存储按你的供应商方式备份（MinIO 用 `mc mirror`，S3 用版本控制或跨区复制）。

### 怎么恢复

```bash
docker exec <pg-容器> psql -U moduless -d postgres -c 'CREATE DATABASE moduless_restored'
docker exec -i <pg-容器> pg_restore -U moduless -d moduless_restored < moduless-2026-08-16.dump
```

**恢复到一个更早的 schema 时，必须同时把 Core 版本退回去。**

这一条反直觉，而且很容易在事后才发现：Core 启动时**无条件**执行内嵌的迁移（`db.InitDB` 里直接调 `RunMigrations`，没有跳过的开关）。所以恢复一份旧转储之后启动当前版本的 Core，它会立刻把 schema 迁回最新 —— 你刚刚恢复的那次回退当场被撤销。

正确的顺序是：

1. 停掉 Core
2. 恢复数据库
3. **启动与该转储 schema 匹配的那个 Core 镜像**（而不是 `latest`）
4. 确认无误后，再逐步升到新版本

### 能不能只回滚 schema

部分可以。`000008` 之前的迁移**不可回滚** —— 它删掉了旧的 extension 相关表，重建只会得到空壳而不是数据，所以它会明确拒绝并提示从备份恢复。它之后的迁移可以正常来回滚（`tests/migrations_test.go` 每次都验证这一点）。

也就是说：**回滚 schema 能解决「刚升级完发现新迁移有问题」，解决不了「想回到几个版本之前」** —— 后者只能靠备份。

## 升级 Core

```bash
docker compose pull core && docker compose up -d core
```

Core 重启会冷启动所有插件（几百毫秒到一秒级，取决于插件数量和它们的初始化工作）。这是单实例设计的代价。数据库迁移在启动时自动执行。

## 一套真实安装的成本

六个示例装在同一个 Core 里（其中五个在请求路径上有 filter），实测：

**内存**：6 个**不同的**二进制，共 109.6 MB，**每个 18.3 MB**。这印证了单插件测量里那条注意事项 —— 那次是 20 份同一个二进制，共享代码段会让 RSS 重复计入，而真实部署里插件是不同的二进制。现在按不同二进制测，数字仍是 18–19 MB，所以那个数可以直接用来做容量规划。

**延迟**（每档 300 次请求）：

| 这个请求做了什么 | p50 | p99 |
|---|---|---|
| 没有插件拥有这条路由：只跑匹配上的 filter | 124 µs | 360 µs |
| 一次插件调用，没用任何 Host 能力 | 374 µs | 1.46 ms |
| ＋ 一次经反向通道的 `Count` | 1.23 ms | 2.07 ms |
| ＋ 一次经反向通道的列表查询 | 1.49 ms | 2.70 ms |

读法：

- **框架本身的开销在第一行**：一个不落到任何插件的请求，跑完全部匹配上的 filter 是 124 µs。
- **一次插件调用约 250 µs**（第二行减第一行），这和单独测出的跨进程往返一致。
- **一次经反向通道的数据库操作约 850 µs**（第三行减第二行）—— 这是插件 → Core → PostgreSQL → Core → 插件的完整来回。**它比插件调用本身贵三倍**，所以热路径上要缓存（[`apikey` 示例](../extension-example/apikey)就是为这个做的两层缓存）。

第三行第一次写下来时我标的是「无数据库操作」—— 而 `notes /stats` 实际会走一次 `Count`。**标错的一行会变成一个错误结论**（「notes 慢」而不是「反向通道的数据库往返要一毫秒」），所以这里每一行标的是它真正做了什么。

### 反向通道的这笔钱花在哪

上表里「一次经反向通道的数据库操作约 850 µs」这个数需要拆开，否则会被拿去优化错的地方。同一个操作在四个深度上测：

| | p50 |
|---|---|
| 一次数据库操作，不经任何框架 | 165 µs |
| ＋ host service（权限检查、类型转换），进程内 | 与上一行**无法区分** |
| 一次不带任何能力的插件调用 | 36 µs |
| **一次经反向通道的数据库操作** | **402 µs** |

减出来：

- **host service 是免费的。**权限检查和 wire 类型转换的开销小到测不出来 —— 所以「减少权限检查」不是可优化的方向。
- **反向通道约 237 µs**（402 − 165）。这比正向的插件调用（36 µs）贵**六倍以上**，原因没有进一步测量，所以这里只记数字不给解释。
- 一个插件的数据库调用，总成本是 Core 直接查同一条语句的 **2.4 倍**。

对写插件的人：**省掉一次 `sdk.DB` 调用，比省掉一次插件调用值钱六倍。**这就是为什么热路径要在进程内缓存（[`apikey` 示例](../extension-example/apikey)），以及为什么批量写要用 `BatchWrite` 而不是循环 `Put`。

## 一个插件变慢时

后端调用有 30 秒上界（`DefaultBackendTimeout`），**但同时在飞的调用数没有上限** —— 事务有 `MaxOpenTxPerPlugin` 兜着，HTTP 请求没有对应的东西。所以在途数是「到达率 × 延迟」，只被那 30 秒界住。

实测两种形状，结论不同：

**闭环负载**（每个调用者等到响应再发下一个，多数客户端是这样）。200 个并发调用者打一个每请求睡 150ms 的插件，12 秒：

| 时间 | 在途 | goroutine | 堆 | 完成/s |
|---|---|---|---|---|
| 2s | 200 | 1024 | 9.6 MB | 1197 |
| 6s | 200 | 1030 | 10.0 MB | 1263 |
| 10s | 200 | 1030 | 9.9 MB | 1276 |

全部持平，**零失败**。在途数天然等于调用者数，涨不上去。

**开环到达**（不等响应就继续发）才是危险的那种。150 个同时到达打一个完全不响应的插件，在途数就是 150 —— 超时之后全部释放。算术是清楚的：**默认 30 秒 + 1000 req/s，意味着一个停止响应的插件会先攒下三万个在途调用，第一个才被放弃。**

所以高流量部署要把 `BackendTimeout` 调小到「一个健康插件的最坏响应时间」，而不是留着 30 秒的默认值 —— 那个默认值是给开发环境的宽容，不是给生产的保护。

## 排查

下面每一行都实测过，除了明确标注的那一条。这不是客套 —— 本仓库三条「最要紧的规则」的失败描述实测下来全部不准，**关于失败的断言比关于成功的断言更容易长期错着没人发现**。

| 现象 | 多半是 |
|---|---|
| 插件不出现在列表 | 目录名与 manifest 的 `key` 不一致，或 manifest 校验失败 —— 控制台会显示原因 |
| `handshake failed: Unrecognized remote plugin message: <某行文字>` | 插件在 `sdk.Serve` 之前往 stdout 写了东西，顶替了握手。后半句就是它打的那行 |
| `exec format error` | 架构不符（例如在 arm64 上跑 amd64 二进制） |
| `no such file or directory`，但文件确实在 | 插件是动态链接的（漏了 `CGO_ENABLED=0`）。缺的是动态链接器不是二进制，内核只能返回 ENOENT。用 `docker run --rm -v "$PWD:/x" alpine ldd /x/bin/plugin` 确认 |
| 插件调用某能力报 PermissionDenied | manifest 的 `permissions` 里没声明它 |
| 插件调用某能力报 Unavailable | Core 没配那项能力（比如没有 `DATABASE_URL`） |
| 部署时 `Text file busy` | 用 `cp` 覆盖了正在执行的二进制。Linux 会拒绝这次写入（ETXTBSY），所以是部署失败而不是插件损坏 —— 改用 `mv`，见上文 |
| 插件不在列表且没有原因 | 不该发生 —— 目录名不符和 manifest 无效都会带原因显示（`tests/deployment_test.go` 断言了这一点）。真遇到就是 bug |
| 控制台菜单不更新 | 多半是 SSE 流被中间代理缓冲；确认反向代理没有缓冲 `text/event-stream`。（这一条**没有实测过**，是唯一没被验证的一行）|

## 数据库重启

Core 不需要跟着数据库一起重启。连接池会自己重连，实测在 docker compose 上重启 PostgreSQL：

- 写请求在约 1 秒内失败，错误是 `the database system is starting up`
- 之后自动恢复，无需人工干预
- 三个插件全部存活，没有被隔离，Core 没有重启

中断期间的行为是**降级而不是死亡**：需要数据库的能力返回明确错误，不需要数据库的路由照常服务。这也意味着中断期间基于 `log` 阶段的审计记录会丢 —— 那是 fail-open 的既定后果，见插件开发指南里关于审计完整性的说明。
