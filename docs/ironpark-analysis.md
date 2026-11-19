# ironpark/acp-go 北向协议实现完整性分析

> 分析日期：2026-11-19
> 分析对象：[github.com/ironpark/acp-go](https://github.com/ironpark/acp-go)（本地 clone 于 `/home/harness/app/acp-go`）
> 对照基准：ACP 官方规范（`/home/harness/app/agent-client-protocol` 的 `schema/v1/meta.json`）

## 结论（TL;DR）

ironpark/acp-go 的北向（agent 侧）ACP v1 协议实现**完整无缺**：

- 方法覆盖 **100%**（agent 13/13、client 11/11、协议级 `$/cancel_request` 1/1）
- 内容类型完整（`SessionUpdate` 联合类型含 v1 全部 20 个变体）
- 额外实现了 11 个 v1 unstable（draft）方法
- 附带了 coder 没有的 WebSocket/HTTP transport（`acphttp`）和 v2 支持（`acp2`）

唯一需要留意的「性质」问题：ws 承载是 draft RFC，且该库是**非官方**实现。协议消息本身是稳定 v1，不受影响。

---

## 1. 方法覆盖对照

### 1.1 agent 要处理的方法（client → agent）：13/13 ✅

| 官方方法名 | ironpark 接口 | 是否必需 |
|---|---|---|
| `initialize` | `Agent.Initialize` | 必需 |
| `session/new` | `Agent.NewSession` | 必需 |
| `session/prompt` | `Agent.Prompt` | 必需 |
| `session/cancel` | `Agent.CancelSession` | 必需 |
| `authenticate` | `Authenticator` | 可选 |
| `session/load` | `SessionLoader` | 可选 |
| `session/list` | `SessionLister` | 可选 |
| `session/delete` | `SessionDeleter` | 可选 |
| `session/resume` | `SessionResumer` | 可选 |
| `session/close` | `SessionCloser` | 可选 |
| `session/set_mode` | `SessionModeSetter` | 可选 |
| `session/set_config_option` | `SessionConfigOptionSetter` | 可选 |
| `logout` | `LogoutHandler` | 可选 |

> 核心接口 `Agent` 只要求 4 个方法（Initialize / NewSession / Prompt / CancelSession），
> 其余 9 个各是独立可选接口，实现哪个就支持哪个，不实现则返回 `-32601`（method not found）。

### 1.2 agent 要调 client 的方法（agent → client）：11/11 ✅

| 官方方法名 | ironpark 接口 | 是否必需 |
|---|---|---|
| `session/update` | `Client.SessionUpdate` | 必需 |
| `session/request_permission` | `Client.RequestPermission` | 必需 |
| `fs/read_text_file` | `FileReader` | 可选 |
| `fs/write_text_file` | `FileWriter` | 可选 |
| `terminal/create` | `TerminalHandler` | 可选 |
| `terminal/output` | `TerminalHandler` | 可选 |
| `terminal/release` | `TerminalHandler` | 可选 |
| `terminal/wait_for_exit` | `TerminalHandler` | 可选 |
| `terminal/kill` | `TerminalHandler` | 可选 |
| `elicitation/create` | `ElicitationHandler` | 可选 |
| `elicitation/complete` | `ElicitationHandler` | 可选 |

### 1.3 协议级方法：1/1 ✅

- `$/cancel_request`（JSON-RPC 标准取消）在 `internal/jsonrpc/conn.go` 实现：
  - 本地 ctx 取消时自动向对端发送
  - 收到时由连接层自行应答，不透传给 handler
  - 未知 id 忽略（符合 RFC）

### 1.4 额外实现的 v1 unstable 方法（draft RFC）：11/11 ✅

| 官方方法名 | ironpark 接口 |
|---|---|
| `providers/list` / `providers/set` / `providers/disable` | `ProviderManager` |
| `mcp/message` | `MCPMessageHandler` |
| `session/fork` | `SessionForker` |
| `session/archive` / `session/unarchive` | `SessionArchiver` |
| `session/status` | `SessionStatusReader` |
| `session/options` | `SessionOptionsReader` |
| `session/set_title` | `SessionTitleSetter` |
| `auth/status` | `AuthStatusReader` |

> 这些属于 v1 的 draft 扩展，随时可能变；不实现不影响稳定协议。

---

## 2. 内容类型完整性（流式输出的关键）

`SessionUpdate` 联合类型含 **ACP v1 全部 20 个变体**：

```
SessionUpdateUserMessageChunk      SessionUpdateAgentMessageChunk
SessionUpdateAgentThoughtChunk     SessionUpdateToolCall
SessionUpdateToolCallUpdate        SessionUpdatePlan
SessionUpdatePlanUpdate            SessionUpdatePlanRemoved
SessionUpdateAvailableCommandsUpdate SessionUpdateCurrentModeUpdate
SessionUpdateConfigOptionUpdate    SessionUpdateSessionInfoUpdate
SessionUpdateUsageUpdate           SessionUpdateNotice
SessionUpdateCompactionUpdate      SessionUpdateCompactionSummaryChunk
SessionUpdatePromptSuggestionsUpdate SessionUpdateSubagentUpdate
SessionUpdateSessionMessage        SessionUpdateSessionMessageChunk
SessionUpdateUnknown
```

其余核心类型也齐全：`ContentBlock`、`StopReason`、`PlanEntry`、`PermissionOption`、
`RequestPermissionRequest/Response`、以及各类 `*Capabilities`（能力协商）。

---

## 3. 其他佐证

| 项 | 值 |
|---|---|
| 锁定 schema 版本 | **v1.32.0**（与 coder/acp-go-sdk 一致，当前最新 v1 schema） |
| 类型总数 | 170 个 |
| 会话管理辅助 | `SessionManager` / `MemoryStore` / `FileStore` / `NewSessionStream` 开箱即用 |
| 进程辅助 | `acp1.SpawnAgent`（起子进程 ACP agent，harnessd 不用，但说明能力齐全） |
| 双版本 | `acp2`（v2 draft）+ `router.ProtocolRouter`（v1/v2 自动协商） |

---

## 4. 注意事项（非「缺失」，是「性质」）

| 点 | 说明 |
|---|---|
| **ws 承载是 draft** | `acphttp` 实现的是 draft RFC「Streamable HTTP & WebSocket Transport」；协议消息是稳定 v1，但 ws 承载本身不是官方稳定 spec |
| **非官方实现** | README 自述 "This is an unofficial implementation" |
| **命名约定不同** | ironpark 用 `SessionUpdateAgentMessageChunk` 这类带前缀的名字，官方文档写 `AgentMessageChunk`；对照时要映射 |
| **unstable 方法会变** | fork/archive/providers 等属于 draft，随时可能变 |
| **schema 是快照** | pin 在 1.32.0，spec 更新后需 ironpark 重新生成 |

---

## 5. 与 coder/acp-go-sdk 对比

| 维度 | coder/acp-go-sdk | ironpark/acp-go |
|---|---|---|
| ACP v1 协议脏活 | ✅ | ✅ |
| WebSocket transport | ❌（自己写适配器） | ✅ 内置 `acphttp` |
| ACP v2 | ❌ | ✅ `acp2` |
| spawn 子进程 | ❌（自己 `os/exec`） | ✅ `SpawnAgent` |
| 会话管理 | ❌ | ✅ `SessionManager` |
| v1/v2 自动协商 | ❌ | ✅ `router` |
| 维护方 | Coder（公司） | 个人/社区 |

---

## 6. 对 harnessd 的含义

- **北向（对 app）**：ACP v1 协议 + ws 承载，全部交给 ironpark，不用手写。
- **南向（对 codex/claude）**：仍要自己写原生协议客户端 + 翻译层，这部分 ironpark 不提供。

ironpark 的价值：把「对 app 说 ACP」这一整层（协议、消息类型、状态机、取消、能力协商、ws transport）全部省掉，让 harnessd 的开发聚焦在「南向 + 翻译」这两块业务逻辑上。
