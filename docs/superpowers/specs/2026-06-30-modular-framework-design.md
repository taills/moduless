# 跨语言模块化开发框架设计方案 (Multi-Language Modular Framework Spec)

## 1. 概述与设计理念

本框架旨在为团队提供一套**低上手门槛、高代码隔离、部署容器化、调试简易**的模块化开发体系。

### 1.1 核心设计理念
1. **统一入口与暗黑服务 (Zero-Port Exposure)**：除核心网关 (Core/Gateway) 外，所有扩展模块 (Extensions) 在生产和开发环境中**不监听任何 TCP 端口**。扩展通过主动向 Core 发起 gRPC 长连接（连接池）完成注册与双向通信，极大收敛网络安全攻击面。
2. **微前端与资源内存缓存 (In-Memory FE Cache)**：
   * 扩展包含前端 (Qiankun 子应用) 和后端。
   * 生产环境下，扩展启动时通过 gRPC 隧道将前端打包后的静态资源 (`zip` 字节流) 一次性推送到 Core。Core 解压并缓存在**内存 (Memory Map)** 中，直接对外分发静态资源，不产生磁盘 I/O。
   * 开发环境下，前端使用 Vite 开发服务器，Core 动态路由至开发者的本地开发端口，保持 HMR 热更新体验。
3. **流式 HTTP 隧道 (HTTP-over-gRPC Tunnel)**：除了微前端静态文件走内存缓存，其余业务 API 流量和高 I/O 请求（如流式数据、WebSocket）通过双向 gRPC 流切片逆向路由到扩展后端，对浏览器呈现统一的 Core Web 服务。
4. **统一存储与基线文件管理**：文件上传下载作为 Core 基线功能。大文件二进制不经过 gRPC 隧道。
   * **上传**：前端直接上传到 Core 接口，生成 `file_id`。
   * **下载**：扩展通过 SDK 申请临时 Token，将下载链接拼装为 `/api/system/files/download/<file_id>/<temp_token>`，由 Core 统一鉴权并以流式分发，不经过扩展后端。
   * **扩展职责**：仅在数据库中保存和管理 `file_id`，实现极简开发。

---

## 2. 系统整体架构

整个系统由 **Core/Gateway**、**Qiankun 宿主前端**、**多语言 SDK** 以及 **开发者扩展** 组成。

```mermaid
graph TD
    subgraph "Browser (User Client)"
        QiankunHost["Qiankun 宿主 App (Shell)"]
        SubApp["Qiankun 子应用 (UI)"]
    end

    subgraph "Core / Gateway (Docker Container)"
        HttpProxy["HTTP 网关 (Ports: 80/443)"]
        TunnelMgr["隧道管理器 & gRPC 服务 (Port: 9000)"]
        StaticCache["内存静态资源缓存 (Memory Map)"]
        AuthModule["认证与权限拦截器"]
    end

    subgraph "Extension Instance (Go / Java / Python)"
        SDK["Multi-Language SDK"]
        Router["Web 框架 (Gin / SpringBoot / FastAPI)"]
        EmbeddedAssets["内嵌静态资源 (embed / resources)"]
    end

    %% Flow: Loading Frontend
    QiankunHost -->|1. 获取激活扩展列表| HttpProxy
    HttpProxy -->|返回配置| QiankunHost
    QiankunHost -->|2. 加载子应用静态资源| HttpProxy
    HttpProxy -->|直接从内存中读取返回| StaticCache

    %% Flow: Connecting Tunnel & Pushing Static Assets
    SDK -->|3. 建立 gRPC 长连接 (连接池)| TunnelMgr
    SDK -->|4. 一次性推送前端 zip 包| TunnelMgr
    TunnelMgr -->|写入| StaticCache

    %% Flow: API Request Tunneling
    SubApp -->|5. 调用 API /api/extensions/key/*| HttpProxy
    HttpProxy -->|6. 校验 Token & 注入用户 Context| AuthModule
    AuthModule -->|7. 包装为 Protobuf Stream| TunnelMgr
    TunnelMgr -->|8. 通过 gRPC 流分发| SDK
    SDK -->|9. 还原为标准 HTTP/ASGI 传入| Router
```

---

## 3. 协议定义 (gRPC Protobuf)

```protobuf
syntax = "proto3";

package tunnel;

option go_package = "./tunnel";

service ExtensionTunnel {
  // 建立双向通信通道 (多路复用连接池)
  rpc Connect(stream TunnelMessage) returns (stream TunnelMessage);
}

message TunnelMessage {
  string message_id = 1;      // 消息唯一ID，用于并发链路追踪与匹配
  
  oneof payload {
    // === 阶段 1：注册与静态资源上传 (Client -> Server) ===
    RegisterRequest register_req = 2;
    FileChunk file_chunk = 3;
    RegisterComplete register_complete = 4;

    // === 阶段 2：注册确认 (Server -> Client) ===
    RegisterResponse register_resp = 5;

    // === 阶段 3：流式 API 隧道通信 (双向) ===
    HttpRequestChunk http_req_chunk = 6;    // Server -> Client (网关请求)
    HttpResponseChunk http_resp_chunk = 7;  // Client -> Server (扩展响应)

    // === 阶段 4：心跳保持 (双向) ===
    Ping ping = 8;
    Pong pong = 9;
  }
}

// 注册基本信息
message RegisterRequest {
  string extension_key = 1;     // 扩展唯一标识，如 "user-manager"
  string version = 2;           // 版本号
  string display_name = 3;      // 前端菜单显示的名称
  string menu_icon = 4;         // 菜单图标
  string menu_path = 5;         // 前端路由路径，如 "/user-manager"
  uint64 zip_file_size = 6;     // 前端静态资源 zip 包的总大小
  string zip_sha256 = 7;        // 校验和，判断前端文件是否有变化
  bool is_dev = 8;              // 是否为开发模式
  string dev_frontend_url = 9;  // 开发模式下前端 Vite 服务的入口地址
}

// 静态文件分片 (注册阶段)
message FileChunk {
  bytes content = 1;            // 分片二进制数据（每次 64KB）
  uint32 chunk_index = 2;       // 分片索引
}

message RegisterComplete {}

message RegisterResponse {
  bool success = 1;
  string error_message = 2;
  bool skip_upload = 3;         // 是否命中了 Sha256 缓存，跳过静态资源上传
}

// 流式 HTTP 请求切片 (Core -> SDK)
message HttpRequestChunk {
  string stream_id = 1;         // 每次 HTTP 请求的唯一 ID，多路复用关联键
  bool is_first = 2;            // 是否为首包
  bool is_last = 3;             // 是否为尾包
  
  // 仅在 is_first = true 时携带
  string method = 4;            // "GET", "POST", "PUT", "DELETE"
  string path = 5;              // 去除前缀后的子路径，如 "/users/list"
  string query = 6;             // 查询参数，如 "page=1"
  map<string, string> headers = 7; // HTTP 请求头 (包含网关注入的 X-User-Id 等)
  
  bytes body_chunk = 8;         // 每次携带的 Body 切片数据 (32KB)
}

// 流式 HTTP 响应切片 (SDK -> Core)
message HttpResponseChunk {
  string stream_id = 1;
  bool is_first = 2;
  bool is_last = 3;
  
  // 仅在 is_first = true 时携带
  int32 status_code = 4;        // 200, 400, 500
  map<string, string> headers = 5;
  
  bytes body_chunk = 6;         // 响应体切片数据
}

message Ping { int64 timestamp = 1; }
message Pong { int64 timestamp = 1; }
```

---

## 4. 核心与生命周期管理

### 4.1 扩展注册与更新
* **前端资源推送**：非开发模式下，扩展将前端打包资源压缩为 `zip` 格式，分片上传到 Core。
* **Sha256 缓存优化**：Core 对每个扩展存储最近一次成功的 `zip_sha256`。如果注册时 SHA256 匹配，Core 返回 `skip_upload = true`，直接重用内存缓存。

### 4.2 离线卸载缓冲 (Graceful Deregistration)
* **10 秒重连容限**：当 gRPC 长连接由于网络抖动或扩展重启意外中断时，Core 将对应的扩展状态设为 `Degraded`，但不清除静态资源内存缓存。
* **延时销毁**：若 10 秒内扩展成功重新连入，状态恢复为 `Active`。若超过 10 秒未连入，Core 判定扩展下线，彻底清除内存缓存及路由信息。

---

## 5. 多语言 SDK 桥接机制

三种语言的 SDK 核心职责相同：连接 Core -> 注册并推送前端 -> 接收 gRPC `HttpRequestChunk` -> 转换为各自语言原生 Web 容器的请求包 -> 捕获输出 -> 通过 `HttpResponseChunk` 返还 Core。

### 5.1 Go SDK 设计 (支持 Gin / Mux)
* **原理**：将 gRPC 消息转换为 Go 标准库的 `*http.Request`，利用 Go 的 `io.Pipe()` 内存管道实现流式 Body 的实时解包和处理。通过 Mock `http.ResponseWriter` 捕获路由器输出并实时流式推送回 gRPC 隧道。
* **开发接入代码**：
  ```go
  r := gin.New()
  r.GET("/hello", func(c *gin.Context) {
      user := sdk.GetUser(c.Request.Context()) // 从 context 读取用户信息
      c.JSON(200, gin.H{"user": user.UserID})
  })
  sdk.Start(r, config) // 自动阻塞并启动 gRPC 隧道
  ```

### 5.2 Python SDK 设计 (支持 FastAPI / ASGI)
* **原理**：FastAPI 基于 **ASGI (Asynchronous Server Gateway Interface)** 规范。SDK 收到 `HttpRequestChunk` 时，为该 `stream_id` 创建一个 ASGI App 调用的 `scope`（包含路径、方法、Header 等），并开启两个异步 `asyncio.Queue` 模拟 ASGI 的 `receive`（接收请求体）和 `send`（发送响应体）通道。
* **开发接入代码**：
  ```python
  from fastapi import FastAPI, Depends
  from sdk import get_user, UserContext, start_sdk

  app = FastAPI()

  @app.get("/hello")
  async def hello(user: UserContext = Depends(get_user)):
      return {"user": user.user_id}

  start_sdk(app, config)
  ```

### 5.3 Java SDK 设计 (支持 Spring Boot)
* **原理**：Spring Boot 核心是 `DispatcherServlet`。SDK 接收到 `HttpRequestChunk` 后，利用 `io.piped` 将输入流包装为 `HttpServletRequest` 的子类（主要是重写 `getInputStream()` 读方法），并在内部直接调用 Spring Context 的 `dispatcherServlet.service(request, response)` 方法。`response` 则是 mock 的 `HttpServletResponse`，其 `getOutputStream()` 的写入被 SDK 截获并转化为 gRPC 切片返回。
* **开发接入代码**：
  ```java
  @RestController
  @RequestMapping("/hello")
  public class HelloController {
      @GetMapping
      public ResponseEntity<Map<String, Object>> hello() {
          UserContext user = SdkContext.getUser(); // 获取网关注入的用户上下文
          return ResponseEntity.ok(Map.of("user", user.getUserId()));
      }
  }
  ```

---

## 6. 统一身份认证与上下文传播

Core 在网关层进行统一鉴权，并在转发 HTTP 请求包的 Header 中携带统一的信息：
* `X-User-Id`: 用户的全局唯一ID
* `X-User-Roles`: 逗号分隔的角色标识
* `X-User-Permissions`: 逗号分隔的权限字

各语言 SDK 内置拦截器或中间件，在桥接时自动解析上述 Header，并绑定到当前请求的局部上下文中（Go 的 `context.Context`、Python 的 `asyncio.ContextVar`、Java 的 `ThreadLocal`），使得业务开发人员可用极其简单安全的形式获取用户信息。

---

## 7. 文件管理与授权下载规范

### 7.1 文件上传
* 前端通过 `/api/system/files/upload` 直传给 Core。
* 成功后获得 `file_id`。前端将 `file_id` 填入表单，再通过 API 发给扩展后端存储。

### 7.2 文件授权下载 (路径参数规范)
为了保障安全性且避免 Query 参数引起的兼容性/排版问题，临时授权下载采用完全的**路径参数 (Path Parameters)** 形式：
1. 扩展在返回详情给前端时，通过 SDK 调用 Core RPC 生成下载令牌：
   ```go
   tempToken, _ := sdk.GenerateDownloadToken(ctx, fileID, 300) // 5分钟有效
   ```
2. 扩展拼装下载地址为：
   `/api/system/files/download/<file_id>/<temp_token>`
3. 前端使用该 URL 即可直接通过 Core 下载文件，Core 网关层自动校验签名。**整个 URL 不包含 `?`, `&` 或 `=` 等特殊字符**。

---

## 8. 示例项目目录规范

官方提供的快速上手示例仓库位于 `extension-example` 目录下，按以下语言和前后台职责进行划分：

```
extension-example/
├── go/
│   ├── frontend/             # Qiankun 微前端子应用 (Vue/Vite)
│   └── backend/              # Go 后端应用 (使用 Go SDK + Gin)
├── python/
│   ├── frontend/             # Qiankun 微前端子应用 (React/Vite)
│   └── backend/              # Python 后端应用 (使用 Python SDK + FastAPI)
└── java/
    ├── frontend/             # Qiankun 微前端子应用 (Vue/Vite)
    └── backend/              # Java 后端应用 (使用 Java SDK + Spring Boot)
```

每个语言分类的 `README.md` 包含该语言在本地拉起 Docker Core、运行 `frontend`（npm run dev）和在 IDE 中 Debug 运行 `backend` 的详细指引。
