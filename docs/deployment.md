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
| `PLUGIN_DEV_MODE` | 关 | 跳过 `Pdeathsig`，**仅开发用** |
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

热更新时旧版本仍在处理请求，直到切换提交。**覆盖一个正在执行的二进制会破坏那个进程的内存映像。**

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

插件进程与 Core 在同一个容器里，所以给这个 Pod 的 CPU 和内存要覆盖 Core 加上所有插件。每个 Go 插件进程大约 10–20 MB 起步。

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

## 升级 Core

```bash
docker compose pull core && docker compose up -d core
```

Core 重启会冷启动所有插件（几百毫秒到一秒级，取决于插件数量和它们的初始化工作）。这是单实例设计的代价。数据库迁移在启动时自动执行。

## 排查

| 现象 | 多半是 |
|---|---|
| 插件不出现在列表 | 目录名与 manifest 的 `key` 不一致，或 manifest 校验失败 —— 控制台会显示原因 |
| 插件启动失败且日志无信息 | 插件向 stdout 写了东西，破坏了启动握手 |
| `exec format error` | 插件不是静态链接（漏了 `CGO_ENABLED=0`）或架构不符 |
| 插件调用某能力报 PermissionDenied | manifest 的 `permissions` 里没声明它 |
| 插件调用某能力报 Unavailable | Core 没配那项能力（比如没有 `DATABASE_URL`） |
| 升级后插件行为异常 | 用 `cp` 覆盖了正在执行的二进制，见上文 |
| 控制台菜单不更新 | SSE 流被中间代理缓冲了；确认反向代理没有缓冲 `text/event-stream` |
