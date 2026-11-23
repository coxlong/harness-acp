# harness-acp

`harnessd` 是一个基于 Go 的 ACP v2 网关，支持 Claude Code 和 Codex。

## 构建

```bash
go build -o harnessd ./cmd/harnessd
```

## 运行

```bash
./harnessd --version
./harnessd
```

### Codex

将 Codex CLI 0.171.0 或更新版本加入 `PATH`。启动后，`GET /v1/agents` 会返回通过握手的 Codex，客户端可以连接 `/acp/codex`。本地 ACP 客户端也可以用 stdio 启动。

```bash
./harnessd --stdio --agent codex
```

`harnessd` 通过一个共享的 `codex app-server` 子进程访问 Codex。模型、API 提供商和凭据来自运行 `harnessd` 的服务账号及其 Codex 配置。使用自定义 Responses API 时，把提供商的 `env_key` 指定的环境变量注入服务。已保存的会话仍由 Codex 管理，恢复会话会读取 Codex 持久化的历史。

Codex 的 `request_user_input` 会转成 ACP v2 表单。客户端须在初始化时声明 `capabilities.elicitation.form`。同一会话有多个连接时，表单只发给最近挂接且支持表单的客户端。

实时 `session/update` 通知的 `params._meta["_harness/replayCursor"]` 携带续传游标。客户端保留已经应用的对话状态后，可把最后一个游标原样作为 `session/resume.replayFrom`，补收之后的事件。缓冲覆盖或进程重启后，旧游标返回 `-32602`，客户端可以用 `{"type":"start"}` 重新加载完整历史。

## 目录结构

```
harness-acp/
├── cmd/harnessd/       # 可执行入口
├── internal/version/   # 版本信息
├── go.mod
└── README.md
```
