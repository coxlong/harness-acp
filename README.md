# harness-acp

`harnessd` 是一个基于 Go 的 Agent Client Protocol (ACP) 守护进程（当前为最小骨架）。

## 构建

```bash
go build -o harnessd ./cmd/harnessd
```

## 运行

```bash
./harnessd --version
./harnessd
```

## 目录结构

```
harness-acp/
├── cmd/harnessd/       # 可执行入口
├── internal/version/   # 版本信息
├── go.mod
└── README.md
```
