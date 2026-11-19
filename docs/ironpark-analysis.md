# ironpark/acp-go 北向协议实现完整性分析（ACP v2）

> 分析日期：2026-11-19
> 分析对象：[github.com/ironpark/acp-go](https://github.com/ironpark/acp-go)（本地 clone 于 `/home/harness/app/acp-go`）
> 对照基准：ACP 官方规范（`/home/harness/app/agent-client-protocol` 的 `schema/v2/meta.json`）
>
> **定案：北向（app ↔ harnessd）走 ACP v2 + WebSocket transport。**

## 结论（TL;DR）

ironpark/acp-go 的北向（agent 侧）**ACP v2** 实现同样**完整无缺**：

- 方法覆盖 **100%**（agent 13/13、client 4/4、协议级 `$/cancel_request` 1/1）
- 内容类型完整（`SessionUpdate` 联合类型含 v2 全部 28 个变体）
- 附带 v1/v2 自动协商（`router`）和 WebSocket/HTTP transport（`acphttp`）

需要接受的「性质」问题：**v2 本身是 draft**（schema `2.0.0-alpha.16`，API 和线上格式可能变），且 ws 承载是 draft RFC、该库是非官方实现。协议消息按 v2 draft，不因承载而变。

---

## 1. v2 方法覆盖对照

### 1.1 agent 要处理的方法（client → agent）：13/13 ✅

| 官方方法名 | ironpark 接口 | 是否必需 |
|---|---|---|
| `initialize` | `Agent.Initialize` | 必需 |
| `session/new` | `Agent.NewSession` | 必需 |
| `session/prompt` | `Agent.Prompt` | 必需 |
| `session/cancel` | `Agent.CancelSession` | 必需 |
| `session/list` | `Agent.ListSessions` | 必需 |
| `session/resume` | `Agent.ResumeSession` | 必需 |
| `session/close` | `Agent.CloseSession` | 必需 |
| `session/set_config_option` | `SessionConfigOptionSetter` | 可选 |
| `session/delete` | `SessionDeleter` | 可选 |
| `auth/login` | `AuthHandler.Login` | 可选 |
| `auth/logout` | `AuthHandler.Logout` | 可选 |
| `tool_call/cancel` | `ToolCallCanceller` | 可选 |
| `terminal/cancel` | `TerminalCanceller` | 可选 |

> v2 的必需接口 `Agent` 有 **7 个方法**（比 v1 的 4 个多了 ListSessions / ResumeSession / CloseSession）。
> 其余 6 个各是独立可选接口，不实现则返回 `-32601`。

### 1.2 agent 要调 client 的方法（agent → client）：4/4 ✅

| 官方方法名 | ironpark 接口 | 是否必需 |
|---|---|---|
| `session/update` | `Client.SessionUpdate` | 必需 |
| `session/request_permission` | `Client.RequestPermission` | 必需 |
| `elicitation/create` | `ElicitationHandler` | 可选 |
| `elicitation/complete` | `ElicitationHandler` | 可选 |

> 注意：v2 把 v1 的 `fs/read_text_file`、`fs/write_text_file`、`terminal/*`（共 7 个）移出了基础 client 方法，
> 文件系统和终端在 v2 里改走 `session/update` 的对应变体（见下）。

### 1.3 协议级方法：1/1 ✅

- `$/cancel_request` 在 `internal/jsonrpc/conn.go` 实现：ctx 取消自动发、收到自应答、未知 id 忽略。

---

## 2. v2 内容类型完整性

`SessionUpdate` 联合类型含 **v2 全部 28 个变体**（相比 v1 新增了终端、状态、replay、custom 等）：

```text
SessionUpdateUserMessage            SessionUpdateUserMessageChunk
SessionUpdateAgentMessage           SessionUpdateAgentMessageChunk
SessionUpdateAgentThought           SessionUpdateAgentThoughtChunk
SessionUpdateToolCallUpdate         SessionUpdateToolCallContentChunk
SessionUpdatePlanUpdate             SessionUpdatePlanRemoved
SessionUpdateAvailableCommandsUpdate SessionUpdateConfigOptionUpdate
SessionUpdateSessionInfoUpdate      SessionUpdateUsageUpdate
SessionUpdateNotice                 SessionUpdateCompactionUpdate
SessionUpdateCompactionSummaryChunk SessionUpdatePromptSuggestionsUpdate
SessionUpdateSubagentUpdate         SessionUpdateSessionMessage
SessionUpdateSessionMessageChunk    SessionUpdateTerminalUpdate
SessionUpdateTerminalOutputChunk    SessionUpdateStateUpdate
SessionUpdateReplayComplete         SessionUpdateCustom
SessionUpdateVariant / SessionUpdateVariants / SessionUpdateUnknown
```

> v2 命名同样带前缀（`SessionUpdateXxx`），对应官方文档的裸名。

---

## 3. v1 → v2 的 spec 差异（影响 harnessd 翻译层）

| 变化 | v1 | v2 |
|---|---|---|
| 认证 | `authenticate` + `logout` | 拆成 `auth/login` + `auth/logout` |
| 会话加载 | `session/load` | 移除（用 `session/resume` 取代） |
| 会话模式 | `session/set_mode` | 移除 |
| 新增取消粒度 | — | `tool_call/cancel`、`terminal/cancel` |
| client 文件系统 | `fs/read_text_file`、`fs/write_text_file` | 移除（并入 plan/session 机制） |
| client 终端 | `terminal/*` 5 个方法 | 移除（改为 `session/update` 的 `TerminalUpdate` / `TerminalOutputChunk`） |
| 必需 Agent 方法 | 4 个 | 7 个 |

---

## 4. 额外实现的 unstable 方法（draft RFC）

v2 也带了同一套 unstable 扩展：`session/fork`、`session/archive`、`session/unarchive`、`session/status`、`session/options`、`session/set_title`、`auth/status`、`providers/*`、`mcp/message`。

---

## 5. 注意事项（非「缺失」，是「性质」）

| 点 | 说明 |
|---|---|
| **v2 是 draft** | schema `2.0.0-alpha.16`，API 和线上格式都可能变，锁定前需接受 |
| **ws 承载是 draft** | `acphttp` 实现 draft RFC「Streamable HTTP & WebSocket Transport」 |
| **非官方实现** | README 自述 "This is an unofficial implementation" |
| **命名约定不同** | ironpark 用 `SessionUpdateAgentMessageChunk` 这类带前缀名字，官方文档写裸名 |
| **schema 是快照** | pin 在 `2.0.0-alpha.16`，spec 更新后需 ironpark 重新生成 |

---

## 6. 对 harnessd 的含义

- **北向（对 app）**：ACP **v2** 协议 + ws 承载，全部交给 ironpark（`acp2` + `acphttp`），不用手写。
- **南向（对 codex/claude）**：仍要自己写原生协议客户端 + 翻译层。
- **翻译层的 v2 差异**：因为北向是 v2，南向 codex/claude 原生事件要映射成 **v2 的 `SessionUpdate` 变体**（如终端走 `TerminalUpdate`/`TerminalOutputChunk`，而不是 v1 的 terminal client 方法）。

ironpark 的价值不变：把「对 app 说 ACP v2」这一整层（协议、消息类型、状态机、取消、能力协商、ws transport）省掉，让 harnessd 聚焦「南向 + 翻译」。

---

## 附：v1 覆盖情况（备用/对比）

若未来需要 v1 兼容（`router` 可同时服务 v1+v2），v1 覆盖同样完整：agent 13/13、client 11/11、`$/cancel_request` 1/1，schema `1.32.0`。
