# harnessd 架构设计

> 状态：定案（待实现；§17 为评审修订记录）
> 更新：2026-11-19（修订 r2）

## 1. 概述

**harnessd** 是一个单 Go 二进制后端服务，把本机的 coding agent（`codex`、`claude`）统一暴露成 **ACP v2** 端点，供前端 app（同机或异机）调用。

**硬约束**

| 约束 | 说明 |
|---|---|
| 单 Go 二进制 | 部署只要求本机装有 `codex` / `claude` CLI |
| 无 Node 转换进程 | 不使用 `claude-agent-acp` / `codex-acp`；原生 → ACP 的翻译由 harnessd 自己实现 |
| 北向只做 ACP v2 | 不实现 v1 |
| 零持久化 | 不引入数据库；会话真相在后端（见 §10） |

## 2. 运行模式

同一套门面与 backend，两种接入方式：

```bash
harnessd serve                      # ws / Streamable HTTP，多 agent、可远程
harnessd --stdio --agent codex      # stdio，单 agent、单连接
```

| 模式 | 用途 | 传输 | agent 选择 | 发现端点 |
|---|---|---|---|---|
| `serve` | 正式部署，app 同机/异机 | WebSocket / Streamable HTTP | URL 路径 `/acp/{name}` | `GET /v1/agents` |
| `--stdio` | 本地桌面 app、官方 TCK 一致性测试、调试 | stdio | `--agent` 指定，仅一个 | 无 |

`--stdio` 的价值：**能接官方 `acp-tck`**（它通过 stdio 启动 agent 做协议一致性校验），让协议实现有客观验收手段。

两种模式共用 `acp2.Agent` 门面与 backend，差异仅在 transport 与 agent 绑定方式。

## 3. 核心模型：三个独立生命周期

harnessd 里存在三个**互不要求生命周期一致**的状态机。这是整个设计的基础，之前「连接绑 agent」的说法是错的。

```text
① 北向连接（app ↔ harnessd，一条 ws socket）
   连上 → initialize → 干活 → 断开
   秒级~小时级；随时断；可多次重连；可多设备并存

② 会话（session）
   创建 → 运行 turn → 空闲/脱离 → resume / close / 过期
   分钟级~天级；跨连接存活；持久单元

③ 南向进程（harnessd ↔ codex / claude，stdio 子进程）
   spawn → ready → busy → 崩溃/重启 → 退出
   分钟级~天级；一个进程可服务多会话（codex），或一会话一进程（claude）
```

**结论：持久寻址键是 `sessionId`，不是 connection。** ws 连接是一次性的、可丢弃的；`connectionId` 最多作为日志标签或「踢掉同 id 旧 socket」的依据，不参与架构。

### 跨生命周期影响（必须显式实现）

| 事件 | 北向连接 | 会话 | 南向进程 |
|---|---|---|---|
| ws 断开 | 连接销毁，facade 丢弃 | **不变**，turn 继续跑，事件进缓冲 | **不变** |
| app 重连 | 新连接、新 facade | `session/resume` 重新 attach，按 cursor 回放 | 不变 |
| 会话 TTL 到期 | 无影响 | 标记过期、释放缓冲、保留元数据 | 独占型（claude）退出；共享型（codex）不动 |
| 南向进程崩溃 | 已连接的 app 收到错误 | 绑定它的会话标记 failed | 重启策略决定是否重建 |
| 南向进程重启 | 无直接事件 | 需 `ThreadResume` / `--resume` 重建原生会话 | 新进程 |
| `session/close` | 无影响 | 释放资源 | 独占型退出 |
| 同一会话并发 prompt | 并入正在跑的 foreground work（v2 语义，见 §6.1） | 单 turn 互斥，后到 prompt 作为 steer 并入 | 只跑一个 turn |
| 多个连接 attach 同一会话 | 均为订阅者；权限请求只发给最近 attach 的一个 | 订阅者列表 | 不变 |

## 4. 分层与项目结构

```text
┌────────────────── 进程级（harnessd 活着就活着）──────────────────┐
│                                                                │
│  registry      静态 + 探测：支持哪些 agent、装了没、版本          │
│  Backend × N   每个 agent 一个进程级单例，自己管进程模型          │
│  SessionRegistry   sessionId → {handle, 状态, 事件缓冲, 订阅者}  │
│                                                                │
└───────────────────────────┬────────────────────────────────────┘
                            │ 只被引用 / 委托
┌───────────────────────────▼────────────────────────────────────┐
│  连接级（每条 ws / stdio 连接一份）                              │
│                                                                │
│  AgentFacade (acp2.Agent)   无状态，连接建立时新建，断开即销毁     │
│  所有方法 = 查 SessionRegistry → 委托给对应 backend handle        │
└────────────────────────────────────────────────────────────────┘
```

**铁律 1**：`acphttp.NewServer(serve)` 的 `serve` 回调**每条连接调用一次**，所以那里创建的 `acp2.Agent` 天生是连接级的。它必须无状态——一旦把会话存进 facade 结构体，ws 一断会话就死，`session/resume` 永远不可能工作。

**铁律 2：turn 归会话层，不归连接。** acp-go 的 `TurnTracker`（`SessionManager.StartTurn/JoinTurn` 所用）会通过 `context.AfterFunc(conn, cancel)` 在连接关闭时取消 turn（`turn.go:start`）。这与「ws 断开 turn 继续跑」直接冲突，因此：**不使用 acp-go 的 turn 管理**，turn 的启停、取消、互斥全部由 `session/` 持有，facade 只负责「发起 / 订阅 / 转发取消」。可复用 `SessionManager` 的会话表与 list 分页，但不得调用其 turn 相关方法。

> 命名说明：之前讨论的 `BackendPool` 收敛为「**每个 agent 一个进程级单例 Backend**」+「**Backend 产出的会话级 SessionHandle**」，不再单独引入 Pool 概念，避免多一层无实义的抽象。

```text
harness-acp/
├── cmd/harnessd/main.go        # 入口：解析配置 → 探测 agents → 选运行模式
├── internal/
│   ├── config/                 # 配置：监听、token、TLS、agent 定义、安全策略
│   ├── registry/               # agent 注册表 + CLI 探测（LookPath / 版本）
│   ├── acp/                    # 北向
│   │   ├── server.go           #   mux + acphttp + /v1/agents + stdio 模式
│   │   ├── facade.go           #   acp2.Agent 无状态门面
│   │   └── router.go           #   /acp/{name} → Backend
│   ├── session/                # SessionRegistry：会话表、turn 互斥、订阅者、断线补发缓冲、权限仲裁
│   ├── backend/
│   │   ├── backend.go          #   Backend / SessionHandle 接口
│   │   ├── codex/              #   共享 app-server + thread 分发 + 事件翻译
│   │   │   ├── runtime.go      #     进程、JSON-RPC、按 threadId 分发
│   │   │   ├── session.go      #     thread 级会话
│   │   │   ├── translate.go    #     原生事件 → acp2.SessionUpdate
│   │   │   └── history.go      #     ThreadRead → 历史快照（§10.3）
│   │   └── claude/             #   按会话进程 + 空闲回收 + 事件翻译
│   │       ├── runtime.go      #     进程池、回收
│   │       ├── session.go      #     stream-json turn 循环
│   │       ├── translate.go    #     原生事件 → acp2.SessionUpdate
│   │       └── history.go      #     读 transcript：list 与完整历史（§10.3）
│   └── version/
├── docs/
└── go.mod
```

## 5. 北向：ACP v2 服务

### 5.1 选型

| 项 | 选择 |
|---|---|
| 协议 | `github.com/ironpark/acp-go/acp2`（ACP v2 draft，schema `2.0.0-alpha.16`） |
| 传输 | `github.com/ironpark/acp-go/acphttp`（WebSocket + Streamable HTTP，draft RFC） |

`acp2.Agent` 必需方法：`Initialize` / `NewSession` / `ListSessions` / `ResumeSession` / `CloseSession` / `Prompt` / `CancelSession`；client 侧必需 `SessionUpdate` / `RequestPermission`。
可选：`session/set_config_option`、`session/delete`、`auth/login`、`auth/logout`、`tool_call/cancel`、`terminal/cancel`、elicitation。

### 5.2 路由：一个 agent = 一个 URL

```text
GET  /v1/agents                 发现
WS   /acp/codex                 Codex 的 ACP v2 端点
WS   /acp/claude                Claude 的 ACP v2 端点
```

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /v1/agents", discovery.HandleList)

for _, b := range backends {
    mux.Handle("/acp/"+b.Name(), acphttp.NewServer(facadeFor(b)))
}
```

连接天然绑定 agent 名，**无 session 级 agent 路由**。

### 5.3 发现

```json
GET /v1/agents
{
  "harnessdVersion": "0.1.0",
  "acpVersion": 2,
  "agents": [
    { "name": "codex",  "displayName": "Codex",  "endpoint": "/acp/codex",
      "available": true,  "version": "0.169.0" },
    { "name": "claude", "displayName": "Claude", "endpoint": "/acp/claude",
      "available": true,  "version": "2.1.319" }
  ]
}
```

- `available`：`exec.LookPath` + 最低版本校验的结果（不满足则 false）。
- `acpVersion` 明确命名，避免与发现接口自身的 `/v1` 混淆。
- 需要鉴权（见 §11），不默认公开。

### 5.4 版本策略

| 版本 | 落在哪 | 进 URL |
|---|---|---|
| ACP 协议 v2 | `initialize` 带内协商 + `/v1/agents` 的 `acpVersion` | 否 |
| 发现接口版本 | `/v1/agents` 前缀 | 是 |
| harnessd 自身版本 | `/v1/agents` 的 `harnessdVersion` | 否 |
| agent CLI 版本 | `/v1/agents` 每条的 `version` | 否 |

## 6. 南向：Backend 抽象

### 6.1 v2 的 prompt 语义（接口设计的前提）

acp2 的 `Prompt` **在消息被接受时就返回**；turn 结束由 `state_update: idle(stopReason)` 通知，期间新 prompt 可并入正在跑的 foreground work。因此南向接口不能是「`Prompt` 阻塞到 `StopReason`」，而是「发送 + 事件流」。这也正好对应两个原生协议：codex 的 `turn/start` / `turn/steer`、claude 的「往 stdin 写一行」。

```go
// Backend 是「一个 agent」的进程级单例，进程模型由实现自己决定。
type Backend interface {
    Name() string
    Probe(ctx context.Context) (AgentInfo, error)       // 版本、可用性
    // list 与完整历史的数据源在后端（§10.3），harnessd 不存。
    ListSessions(ctx context.Context, f ListFilter) ([]SessionInfo, string, error)
    History(ctx context.Context, id string) ([]acp2.SessionUpdate, error)  // 实体级快照，已含 primer
    NewSession(ctx context.Context, p NewSessionParams) (SessionHandle, error)
    ResumeSession(ctx context.Context, id string, p ResumeParams) (SessionHandle, error)
    Close() error                                        // harnessd 退出时调用
}

// SessionHandle 是会话级句柄：一个 ACP session 对应一个。
// 事件不通过回调返回，而是写入 Events()，由 session/ 消费、编号、扇出。
type SessionHandle interface {
    NativeID() string
    // Send 提交一条用户消息：空闲则开新 turn，运行中则并入（steer）。
    // 返回 messageID（v2 要求保留）。turn 的结束以 Events() 里的 idle 状态事件表示。
    Send(ctx context.Context, blocks []acp2.ContentBlock) (acp2.MessageID, error)
    Events() <-chan Event            // SessionUpdate | PermissionRequest | Fatal
    Respond(reqID string, d Decision) error   // 回应权限请求
    Cancel(ctx context.Context) error
    Close(ctx context.Context) error
}
```

`Event` 里的 `PermissionRequest` 与 `SessionUpdate` 走同一个通道，保证权限请求与其前后的 tool call 更新保持顺序。

两者职责分离的理由：**翻译逻辑是会话级的，OS 进程是进程级的**。codex 一个进程服务多会话，若把进程所有权写进 SessionHandle，第一个会话结束就会误杀进程。

## 7. codex backend：共享一个 app-server 进程

### 7.1 进程模型

- harnessd 启动时（或首次用时）spawn **一个** `codex app-server`，stdio NDJSON JSON-RPC。
- 生命周期 = harnessd 生命周期；所有会话共用。
- 退出处理：独立进程组 + `Pdeathsig`，避免 harnessd 被 kill 后留下孤儿。
- 崩溃：所有线程会话标记 failed → 重启进程 → 会话按需 `ThreadResume` 恢复（thread 本身在 `~/.codex` 持久化）。
- 不使用 `codex app-server daemon`（见 §13 决策记录）。

### 7.2 会话 = thread

| ACP | codex |
|---|---|
| `session/new` | `ThreadStart` → `threadId` |
| `session/resume` | `ThreadResume{threadId, cwd, ...}` |
| `session/list` | `ThreadList{cwd, archived, cursor, ...}`（可直接代理） |
| 完整历史（`replayFrom: start`） | `ThreadRead{includeTurns:true}` → 翻译为实体级快照 |
| 并发 prompt | `TurnSteer`（并入运行中的 turn） |
| `cancel` | `TurnInterrupt` |
| `session/close` | 释放内存中的 thread 绑定（不 Archive） |

`ThreadStartParams` 支持**线程级**的 `cwd`、`model`、`sandbox`、`approvalPolicy`、`modelProvider`、`config`、`developerInstructions` 等——**所以共享进程不破坏会话隔离**，每个会话仍可各有自己的目录与配置。这是「共用一个进程」决定成立的前提。

### 7.3 事件分发（共享进程带来的唯一额外复杂度）

所有事件从**同一条** JSON-RPC 流回来，且都带 `threadId`：

```text
app-server stdout
  → runtime 读一条 → 取 threadId → 查 map[threadId]*codexSession
  → 翻译成 acp2.SessionUpdate → 交给 SessionRegistry（广播给订阅者 / 入缓冲）
```

- 查不到 threadId 的事件：记录日志后丢弃（可能是其他客户端的线程）。
- 每个会话有独立的 outbound 队列，**慢消费者不阻塞其他会话**。队列写满时策略：**断开该订阅者的连接**（让它走重连 + 补发），不丢帧、不阻塞后端。

### 7.4 事件翻译

| codex 事件 | → acp2.SessionUpdate |
|---|---|
| `AgentMessageDeltaNotification` / `AgentMessage` | `SessionUpdateAgentMessageChunk` / `SessionUpdateAgentMessage` |
| `ReasoningTextDeltaNotification` / `ReasoningSummaryTextDeltaNotification` | `SessionUpdateAgentThoughtChunk` |
| `ItemStartedNotification` / `ItemCompletedNotification` | `SessionUpdateToolCallUpdate`（+ `ToolCallContentChunk`） |
| `TurnPlanUpdatedNotification` | `SessionUpdatePlanUpdate` |
| `ThreadTokenUsageUpdatedNotification` | `SessionUpdateUsageUpdate` |
| 终端类事件 | `SessionUpdateTerminalUpdate` / `SessionUpdateTerminalOutputChunk` |
| `ThreadStartedNotification` | `SessionUpdateSessionInfoUpdate` |

### 7.5 类型来源

`codex app-server generate-json-schema --out <dir>` 可导出完整协议 schema（v1 + v2，约 4.7MB）。构建期生成 Go 类型，**不手写 200+ 结构体**：

```makefile
codex-schema:
	codex app-server generate-json-schema --out internal/backend/codex/schema
	go generate ./internal/backend/codex/...
```

版本不符时生成会失败——这本身就是一道版本门槛。

## 8. claude backend：每会话进程 + 空闲回收

### 8.1 进程模型（混合）

**session uuid 是真相，进程是缓存。**

```text
                首次 prompt
                     │ spawn: --session-id <uuid>
                     ▼
              ┌──────────────┐  turn 结束（stdin 保持打开）
              │ 长驻进程      │ ────────────┐
              │ （本会话）     │ ◄───────────┘ 下一轮直接写 stdin
              └──────┬───────┘
                     │ 空闲 TTL / 进程崩溃 / harnessd 重启
                     ▼ kill 进程，只保留 uuid
              spawn: --resume <uuid>   ← 冷路径，上下文从磁盘恢复
```

### 8.2 命令行

```bash
# 新会话首轮
claude --print \
       --input-format stream-json --output-format stream-json \
       --include-partial-messages \
       --session-id <uuid> \
       --permission-mode <mode> \
       --add-dir <dir>...

# 冷路径（恢复已有会话）
claude --print \
       --input-format stream-json --output-format stream-json \
       --include-partial-messages \
       --resume <uuid> \
       --permission-mode <mode> --add-dir <dir>...
```

已实测：`--resume` 与 `--input-format/-output-format stream-json` 可组合。

**待 M0 验证**：权限回路所需的 flag（当前 CLI 有 `--permission-prompts host|none`，默认 `host`，另有 `--permission-prompt-tool`）。8.2 命令行尚未包含它，`control_request(can_use_tool)` 在 `--print` + stream-json 下的触发条件以实测为准（参考 `claude-agent-acp` 对 SDK 的用法）。

### 8.3 turn 循环

- 一轮 = 往 stdin 写**一行** JSON（用户消息）。
- 结束判据 = stdout 出现 `{"type":"result", ...}`（含 `session_id`、`stop_reason`、`usage`）。
- 进程在 `result` 之后**保持存活**等待下一次输入，这是长驻模式的基础。

### 8.4 单进程互斥（必须）

`--resume` 的行为是：会话若正被另一进程运行，**会 fork 出一个副本并告知**。所以同一会话绝不能有两个进程同时存在。`session/` 对每个会话持一把互斥量：冷路径 spawn 前先持锁并确认旧进程已退出；一个会话同时只有一个进程、一个 turn。这是普通互斥，不再单独抽象为「租约」。

### 8.5 事件翻译

| claude stream-json | → acp2.SessionUpdate |
|---|---|
| `content_block_delta`(text_delta) / assistant message | `SessionUpdateAgentMessageChunk` |
| `thinking_delta` | `SessionUpdateAgentThoughtChunk` |
| `content_block_start/stop`(tool_use) / tool_result | `SessionUpdateToolCallUpdate` |
| 任务/计划类输出 | `SessionUpdatePlanUpdate` |
| `result` 的 usage | `SessionUpdateUsageUpdate` |

### 8.6 空闲回收

| 参数 | 建议初值 | 说明 |
|---|---|---|
| 进程空闲 TTL | 30 min（可配） | 到期 kill 进程，保留 uuid |
| 会话元数据 TTL | 无 | list 由扫描 transcript 得出（§10.3），不依赖内存 |

## 9. 权限回路

两条原生协议形态不同，但都归一到 ACP 的 `session/request_permission`。

### 9.1 codex：原生 JSON-RPC 请求

app-server 直接发请求，harnessd 响应即可：

| codex 请求 | ACP 映射 |
|---|---|
| `CommandExecutionRequestApproval` | 命令执行授权 |
| `ExecCommandApproval` | 同上（legacy） |
| `ApplyPatchApproval` | 补丁应用授权 |
| `FileChangeRequestApproval` | 文件变更授权 |

流程：`app-server → runtime → perm 包构造 RequestPermission{title, options} → client.RequestPermission → 决定 → 写回 JSON-RPC 响应`。

### 9.2 claude：stdout 上的控制请求

claude 把权限请求作为 `control_request`（`can_use_tool`）写进 **stdout**，父进程把决定作为 `control_response` 写回 **stdin**。不需要单独的 MCP 进程。

```text
claude stdout: {"type":"control_request","request":{...,"subtype":"can_use_tool",...}}
                                    │
                    harnessd 转成 ACP session/request_permission → app
                                    │
claude stdin:  {"type":"control_response", ...}   ← app 的决定
```

### 9.3 无人在线时的权限处理

权限请求只发给**最近 attach 的**订阅者。无订阅者（断连或无人 attach）时：

- **立即拒绝**（fail-safe），并通知 claude/codex。
- 绝不默认放行；不做「挂起等待重连」的配置项（复杂度高、收益低，需要时再加）。

## 10. 会话生命周期、重连与回放

### 10.1 零持久化

| 数据 | 是否落盘 | 理由 |
|---|---|---|
| `sessionId ↔ native id` 映射 | **否** | 直接令 ACP `sessionId` = 后端原生 id（codex 的 `ThreadId`、claude 由 `--session-id` 指定的 UUID），无映射即无状态 |
| 会话元数据（list） | **否** | codex 代理 `ThreadList`；claude 扫描 `~/.claude/projects/` transcript 目录（见 §10.3） |
| 断线补发缓冲 | **否** | 内存环形缓冲，仅跨连接补发；完整历史由后端提供 |
| 单进程互斥 | **否** | 进程内互斥量 |
| 配置 | 配置文件 | 常规配置，不是运行时状态 |

**原则：harnessd 不拥有会话数据，只拥有「活着的进程」和「内存里的连接/事件状态」。真相在后端。**

harnessd 重启后：app 用原 `sessionId` 调 `session/resume` → harnessd 翻译成 `ThreadResume` / `--resume` → 会话接回，不需要 harnessd 保存过任何东西。

### 10.2 重连

ws 层**没有连接 id，也不可能有**：一条 socket 就是一条连接，断开即终结。

重连 = 新建连接 + ACP `session/resume`。

### 10.3 回放

v2 `session/resume` 的 `replayFrom` 有两个独立用途，对应两套不同的数据来源：

| cursor | 用途 | 数据来源 |
|---|---|---|
| 省略 / `null` | 不回放，从当前继续 | — |
| `{"type":"start"}` | 回放完整历史（全部保留的会话实体） | **后端提供**（见下） |
| `{"type":"_harness/seq","seq":N}` | 断线补发：从第 N 条之后补齐 | 内存环形缓冲 |

**完整历史（`replayFrom: start`）的数据来源**

- **codex**：`ThreadRead{threadId, includeTurns:true}` → 以 turn 为单位遍历，翻译成实体级快照（消息、tool call、plan 各自的当前状态，而非逐 delta 重放）。
- **claude**：扫描 `~/.claude/projects/<cwd-hash>/*.jsonl` transcript，解析后翻译成实体级快照。两个后端均在 `internal/backend/{codex,claude}/history.go` 实现。

这样 harnessd 重启后 `replayFrom: start` 仍然可用，不依赖内存缓冲。

**回放中的 chunk 实体规则**（v2 规范要求）：凡是有 chunk 变体的实体（消息、tool call 内容、终端输出、compaction summary），回放时须先发一条空内容的"primer"更新，再发 chunk，防止客户端将本次 chunk 追加到上次连接已持有的内容上。

**断线补发（`_harness/seq`）**：`SessionRegistry` 给每个活跃会话维护**环形缓冲**（容量可配，如 500 条），每条 `SessionUpdate` 带单调序号。短暂断线后用此游标补发，无需读磁盘。缓冲覆盖不到时返回 `-32602`，让 app 改用 `{"type":"start"}`，不静默跳过。

回放结束发 `SessionUpdateReplayComplete`（无论历史是否为空）。

### 10.4 回收

```text
socket 断开
  ├─ 立即：解绑订阅者、销毁 facade；会话继续跑，事件进环形缓冲
  ├─ 会话空闲 TTL 到期且无订阅者
  │     ├─ 杀独占进程（claude）/ 保留共享进程（codex）
  │     ├─ 释放环形缓冲
  │     └─ 会话 id 仍可 resume（从后端 transcript/thread 重建）
  └─ 死连接检测：acphttp.WithWebSocketPing + acphttp.WithIdleTimeout
```

慢消费者（订阅者的队列写满）：**断开该连接**，让其走重连 + 断线补发，不丢帧、不阻塞后端进程。

## 11. 安全模型

agent 本质是**任意代码执行**，而 app 可能来自异机。默认配置必须保守。

| 项 | 措施 |
|---|---|
| 鉴权 | `Authorization: Bearer <token>`，中间件包住 `/acp/*` **与** `/v1/agents`；ws 升级同样校验 |
| 传输 | TLS（自带证书或反向代理终止）；默认只监听 `127.0.0.1` |
| ws Origin | `acphttp.WithWebSocketOrigins` 白名单，默认只允许同源 |
| cwd 白名单 | `session/new` 的 `cwd` 与 `additionalDirectories` 必须落在配置的允许根内，否则 `-32602` |
| agent 开关 | 每个 agent 可 `enabled: false`，不暴露端点 |
| 权限默认值 | 请求未决超时 → 拒绝；不做无人值守自动批准 |
| 审计 | 每个 prompt、每次权限决定记录（含 sessionId、agent、决定、耗时） |

## 12. 版本与兼容

| 依赖 | 版本策略 |
|---|---|
| ACP v2 | draft（`2.0.0-alpha.16`）；锁定 `acp2` 的 commit/tag，升级需回归 |
| `codex app-server` | **experimental**；设最低版本（如 ≥ 0.169.0），探测不满足则 `available:false` |
| claude CLI | 设最低版本；`stream-json` 与 `control_request` 属内部协议，需契约测试兜底 |

**契约测试**：对两个 CLI 各写一组「录制 → 回放」测试，把一次真实交互的事件流固化成 fixture；CLI 升级后重跑，第一时间发现协议漂移。

## 13. 决策记录

| 决策 | 结论 | 理由 |
|---|---|---|
| ACP 版本 | 北向只用 v2 | 需求方指定；v1 不做 |
| SDK | ironpark/acp-go | 内置 v2 + ws transport，覆盖完整（见 `ironpark-analysis.md`） |
| acp-go SessionManager 复用范围 | 复用会话表与 list 分页；**不用其 turn 相关方法** | `TurnTracker` 在连接关闭时取消 turn，与断线后 turn 继续的核心承诺冲突 |
| Node 转换进程 | 不用 | 要求单二进制 |
| codex 进程 | 共享一个 `app-server` | 线程级参数保证隔离；减少进程数 |
| codex daemon | 不用 | 会话持久化靠 `ThreadResume` 即可；daemon 是机器级共享，带来跨工具干扰、版本漂移、`update` 打断风险 |
| claude 多轮 | 长驻进程 + 空闲回收 + `--resume` 冷路径 | 快路径零冷启动，冷路径保证恢复 |
| claude session/list | 扫描 `~/.claude/projects/` transcript 目录 | 进程内存不跨重启；transcript 由 claude 本身维护，无需 harnessd 落盘 |
| 完整历史数据来源 | 从后端读取（codex: `ThreadRead`；claude: transcript） | 内存缓冲覆盖不到重启；后端本身有完整历史 |
| 持久化 | 无 | 真相在后端；避免 DB/迁移/一致性问题 |
| 会话 id | 等于后端原生 id | 无映射表即无状态 |
| perm/ 单独包 | 不建 | 两边形态不同，共用的只有「构造 RequestPermission + 断连即拒绝」，放进 `session/` 足够 |
| 并发 prompt | 并入运行中 turn（v2 语义） | v2 Prompt 在消息接受时返回；后续 prompt 作为 steer 并入，不拒绝 |

## 14. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| `codex app-server` 标记 experimental | 协议可能破坏性变更 | 生成式类型 + 最低版本门槛 + 契约测试 |
| claude `stream-json` / `control_request` 无公开规范 | 升级即可能失效 | 契约测试；把 CLI 版本钉在 config |
| ACP v2 仍是 draft | 规范变动 | 锁 `acp2` 版本；门面层隔离，变更集中在 `internal/acp/` |
| 共享进程爆炸半径 | codex 崩溃影响所有会话 | 崩溃后全员标记 failed + 自动重启 + `ThreadResume` 恢复 |
| claude 一会话一进程 | 内存压力 | 空闲 TTL 回收；并发上限 |
| 远程暴露 | 任意代码执行 | §11 全套措施，默认只监听本地 |

## 15. 分阶段实施

| 阶段 | 目标 | 验收 |
|---|---|---|
| **M0** | Spike：实测 claude 进程内存、`--resume` 冷启动耗时、`codex app-server` 握手与 thread 流程 | 产出数据，校准 TTL 与并发上限 |
| **M1** | 骨架：config + registry + 探测 + `/v1/agents` + 无状态 facade + `--stdio` 模式 | `GET /v1/agents` 正确；stdio 下 `initialize` 协商 v2 成功；可接 `acp-tck` 跑通基础项 |
| **M2** | codex 打通：共享 app-server、thread 会话、文本流 | 客户端 prompt → 文本流式回传 |
| **M3** | codex 完整：工具调用、计划、usage、权限、取消、`session/list` | 工具与权限可透传；list 对齐 `ThreadList` |
| **M4** | claude 打通：每会话进程、turn 循环、事件翻译 | 文本流、工具调用可用 |
| **M5** | claude 完整：权限控制回路、空闲回收、`--resume` 冷路径 | 崩溃/重启后可恢复；权限可通到 app |
| **M6** | 重连回放：断线补发环形缓冲 + `_harness/seq`；`replayFrom:start` 从后端读取完整历史；`replay_complete` | 断线重连不丢事件（缓冲窗口内）；harnessd 重启后 `start` 仍可用 |
| **M7** | 安全与运维：token、TLS、origin、cwd 白名单、审计日志 | 违例请求全部被拒 |

## 16. 待实测（M0 Spike 清单）

**下列第 4 项最优先**，其结果直接决定 §7.3 的队列设计。

1. `codex app-server` 是否并发处理多个 thread 的 turn，还是串行 → 决定是否需要每会话独立队列（**最高优先**）
2. claude 常驻进程的实际内存占用 → 决定默认空闲 TTL / 并发上限
3. `--resume` 冷启动耗时（会话长短不同）→ 决定是否值得默认常驻
4. stream-json 在 `result` 之后进程等待 stdin 的确切行为（是否有超时 / 心跳）
5. `--permission-prompts host` + `--print` + stream-json 下 `control_request(can_use_tool)` 的确切触发条件（§9.2 的前提，M4/M5 之前需落实）
6. claude `~/.claude/projects/` transcript 目录结构 → 验证 §10.3 的 session/list 实现路径
7. `ThreadList` 的返回是否包含 harnessd 自己创建的全部线程（originators 过滤）

## 17. 修订记录

| 版本 | 日期 | 修订内容 |
|---|---|---|
| r1 | 2026-11-19 | 初版定案 |
| r2 | 2026-11-19 | **设计评审修订**：(1) §6 重写 Backend/SessionHandle 接口，对齐 v2 prompt 语义（消息接受即返回，turn 结束由 idle 事件通知，并发 prompt 并入而非拒绝）；(2) §3 补充「turn 不能随连接取消」铁律，说明不得使用 acp-go TurnTracker；(3) §7.2 补充 TurnSteer / TurnInterrupt 映射，§7.3 明确慢消费者策略（断连而非丢帧）；(4) §8.4 将「租约」改为普通互斥量；(5) §9.3 去掉「挂起等待重连」配置项，改为立即拒绝；(6) §10.1 更新表格，§10.3 拆分完整历史与断线补发两套机制，完整历史由后端提供（codex: ThreadRead，claude: transcript），修正 chunk 实体回放的 primer 规则；(7) §10.4 更新回收流程描述；(8) §13 补充 7 条新决策；(9) §15 M6 更新验收描述；(10) §16 补充 permission 回路和 transcript 目录两个验证项，重排优先级 |
