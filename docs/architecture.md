# harnessd 架构设计

> 状态：定案（待实现）
> 更新：2026-11-19

## 1. 概述

**harnessd** 是一个单 Go 二进制后端服务，把本机的多个 coding agent（`codex`、`claude` CLI）统一暴露成 ACP 端点，供前端 app（同机或异机）通过 ACP v2 + WebSocket / Streamable HTTP 调用。

**硬约束**

- 单 Go 二进制，部署时只要求本机已安装 `codex` / `claude` CLI。
- 不引入任何 Node 转换进程（不用 `claude-agent-acp` / `codex-acp`），原生 → ACP 的翻译由 harnessd 自己实现。
- 北向（对 app）只做 ACP **v2**。

## 2. 总体架构

```text
                    app（未来，ACP v2 客户端）
                         │
            ACP v2 over WebSocket / Streamable HTTP
                         │
   ┌─────────────────────▼─────────────────────────────┐
   │                  harnessd（单 Go 二进制）           │
   │                                                    │
   │  ① 北向：acp2 + acphttp（ironpark/acp-go）          │
   │     · 每个 agent 一个 ACP v2 端点                    │
   │     · 发现端点 /v1/agents                           │
   │  ────────────────────────────────────────────────  │
   │  ② 翻译层：原生事件 → acp2.SessionUpdate（自己写）   │
   │  ────────────────────────────────────────────────  │
   │  ③ 南向：原生协议客户端（自己写）                    │
   │     · codex：spawn `codex app-server`（JSON-RPC）    │
   │     · claude：spawn `claude`（stream-json）          │
   └─────────────────────┬─────────────────────────────┘
                         │ 原生协议（非 ACP）
              ┌──────────┴──────────┐
              ▼                     ▼
        codex CLI               claude CLI
```

## 3. 技术选型

| 项 | 选择 | 理由 |
|---|---|---|
| ACP 协议侧 | ironpark/acp-go 的 `acp2`（v2） | 北向 v2，覆盖完整（见 `docs/ironpark-analysis.md`） |
| 北向传输 | ironpark 的 `acphttp`（WebSocket + Streamable HTTP，draft RFC） | 内置 ws，不用手写承载 |
| 南向 | 自己 `os/exec` + 原生协议客户端 | 去掉 Node adapter |
| 翻译层 | 自己写（codex/claude 各自一个） | 任何 SDK 都不提供，是 harnessd 核心 |

> 详见 [`docs/ironpark-analysis.md`](./ironpark-analysis.md)：v2 方法覆盖 13/13 agent、4/4 client、`$/cancel_request` 1/1，schema `2.0.0-alpha.16`。

## 4. 项目结构

```text
harness-acp/
├── cmd/harnessd/main.go        # 入口：读配置 → 探测 agents → 起 server
├── internal/
│   ├── config/                 # 配置：监听地址、token、agents 定义
│   ├── registry/               # agent 注册表 + 发现
│   │   ├── registry.go         #   注册/查询 agent，维护元数据与可用性
│   │   └── probe.go            #   探测 CLI（exec.LookPath + 取版本）
│   ├── backend/                # 南向：原生协议客户端 + 翻译
│   │   ├── backend.go          #   Backend 接口（统一抽象）
│   │   ├── codex/              #   spawn codex app-server + 事件→v2 翻译
│   │   └── claude/             #   spawn claude stream-json + 事件→v2 翻译
│   ├── acp/                    # 北向：ACP v2 服务
│   │   ├── server.go           #   mux + acphttp + /v1/agents 端点
│   │   ├── agent.go            #   acp2.Agent 门面（绑一个 backend）
│   │   └── router.go           #   /acp/{name} → backend 路由
│   ├── session/                # app sessionId ↔ backend 原生会话 映射
│   └── version/                # 版本信息（dev/commit/date）
└── docs/
```

## 5. 北向：ACP v2 服务

- 每个 agent 一个 `acphttp.Server`，挂到自己的路径 `/acp/{name}`。
- 每个 `/acp/{name}` 是一个**完整的 ACP v2 agent 端点**（同时支持 WebSocket 和 Streamable HTTP）。
- `acp2.Agent` 门面绑定一个 backend，实现 v2 必需方法：`Initialize` / `NewSession` / `Prompt` / `CancelSession` / `ListSessions` / `ResumeSession` / `CloseSession`。

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /v1/agents", registry.HandleListAgents)   // 发现

for _, ag := range registry.Agents() {
    backend := backendFor(ag)                              // codex/claude 后端
    srv := acphttp.NewServer(agentServe(backend))          // 绑好 backend 的门面
    mux.Handle("/acp/"+ag.Name, srv)                       // 每个 agent 一个路径
}
```

## 6. 南向：Backend + 翻译

### 6.1 Backend 接口

```go
type Backend interface {
    Name() string
    Available(ctx context.Context) error   // 探测 CLI 可执行

    NewSession(ctx, cwd string) (sessionID string, err error)
    Prompt(ctx, sessionID string, prompt []acp2.ContentBlock,
           emit func(acp2.SessionUpdate) error) (stopReason acp2.StopReason, err error)
    Cancel(ctx, sessionID string) error
    ListSessions(ctx) ([]SessionInfo, error)
    CloseSession(ctx, sessionID string) error
    Close() error
}
```

### 6.2 翻译层（核心工作量）

每个 backend 内部做两件事：**spawn CLI + 把原生事件翻译成 acp2.SessionUpdate**。

**Codex**：spawn `codex app-server`（stdio JSON-RPC）。

| codex app-server 事件 | → acp2.SessionUpdate |
|---|---|
| `AgentMessageDeltaNotification`(text) | `SessionUpdateAgentMessageChunk` |
| `ReasoningTextDeltaNotification` | `SessionUpdateAgentThoughtChunk` |
| `ItemStartedNotification` / `ItemCompletedNotification` | `SessionUpdateToolCallUpdate` 等 |
| `TurnPlanUpdatedNotification` | `SessionUpdatePlanUpdate` |
| 终端相关事件 | `SessionUpdateTerminalUpdate` / `SessionUpdateTerminalOutputChunk` |

**Claude**：spawn `claude` CLI（`--output-format stream-json --verbose`）。

| claude stream-json 事件 | → acp2.SessionUpdate |
|---|---|
| `content_block_delta`(text_delta) | `SessionUpdateAgentMessageChunk` |
| `thinking_delta` | `SessionUpdateAgentThoughtChunk` |
| `content_block_start/stop`(tool_use) | `SessionUpdateToolCallUpdate` 等 |
| 权限请求 | `RequestPermission`（client 调用） |

> 注意：翻译目标必须是 **v2 的 `SessionUpdate` 变体**（含终端/状态/replay 等 v2 新增变体），不是 v1。

## 7. Agent 发现机制

### 7.1 出带 HTTP 端点（主用）

```text
GET /v1/agents
```

```json
{
  "harnessdVersion": "0.1.0",
  "protocolVersion": 2,
  "agents": [
    {
      "name": "codex",
      "displayName": "Codex",
      "endpoint": "/acp/codex",
      "available": true,
      "version": "0.166.0"
    },
    {
      "name": "claude",
      "displayName": "Claude",
      "endpoint": "/acp/claude",
      "available": false,
      "version": ""
    }
  ]
}
```

- `available` = registry 启动时 `exec.LookPath` 探测结果。
- `version` = CLI 版本（运行探测所得）。

### 7.2 ACP 扩展方法（可选，后续）

ironpark 支持 `ExtMethodHandler`（`_` 前缀），可加 `_harness/agents` 返回同样列表，供「纯 ACP、不碰 HTTP」场景。非第一版必需。

## 8. 路由

**模型：一个 agent = 一个 URL，路由由路径决定，不用参数。**

| 层 | 机制 | 由谁决定 |
|---|---|---|
| 发现 | `GET /v1/agents` → name + endpoint URL | registry |
| 寻址 | `/acp/{name}` 路径 = agent 身份 | URL 本身 |
| session 路由 | 连接内 `app sessionId ↔ backend 原生会话` | session manager |

- 连接天然绑定 agent，**无 session 级 agent 路由**。
- app 流程：`GET /v1/agents` → 拿到 `{codex → /acp/codex}` → 直接连 `ws://host/acp/codex`，零定制。

## 9. 版本策略

| 版本 | 落在哪 | 是否进 URL |
|---|---|---|
| ACP 协议版本 v2 | `initialize` 带内协商 + `/v1/agents` 声明 | ❌ 不进路径 |
| 发现接口版本 | `/v1/agents` 路径前缀 | ✅ |
| harnessd 自身版本 | `/v1/agents` 响应字段 `harnessdVersion` | ❌ |
| agent CLI 版本 | `/v1/agents` 每个条目字段 `version` | ❌ |

> ACP 协议版本按 ACP 惯例在 `initialize` 带内协商，不版本化路径。

## 10. 会话与并发

- 每个 app 连接独立；每个 session 一个 goroutine 跑 Prompt，用 context 管理。
- `session/cancel` → `backend.Cancel` → 取消对应 CLI 的进行中 prompt（context + 发取消给 CLI）。
- v2 的 `tool_call/cancel` / `terminal/cancel` → backend 对应粒度取消（后续版本实现）。

## 11. 分阶段实施

| 阶段 | 目标 | 验收 |
|---|---|---|
| **M1** | 骨架：config + registry + 探测 + acphttp server + `/v1/agents` + 空 acp2.Agent 门面 | `GET /v1/agents` 返回 agents；ws 握手 + initialize 协商 v2 成功 |
| **M2** | 打通 codex：spawn `codex app-server`，prompt + 文本流 | 客户端发 prompt，harnessd 转 codex，文本流回传 |
| **M3** | codex 翻译补全（工具调用/计划/取消/终端） | 工具调用、取消能透传 |
| **M4** | 接 claude：stream-json 解析 + 翻译 | 同样流程跑通 claude |
| **M5** | 多 agent 路由 + 会话管理 + 配置完善 | `/acp/codex`、`/acp/claude` 都可用 |
