package config

import (
	"os"
	"time"
)

// Config is the top-level harnessd configuration.
type Config struct {
	Listen  string // default "127.0.0.1:7800"
	Token   string // bearer token; empty = no auth (dev only)
	TLS     TLSConfig
	Agents  AgentsCfg
	Session SessionCfg
}

type TLSConfig struct {
	CertFile string
	KeyFile  string
}

type AgentsCfg struct {
	Codex  AgentCfg
	Claude AgentCfg
}

type AgentCfg struct {
	Enabled    bool
	MinVersion string   // minimum CLI version required
	AllowDirs  []string // cwd whitelist roots
}

type SessionCfg struct {
	ClaudeIdleTTL     time.Duration // default 30m
	ReplayBufSize     int           // ring buffer capacity, default 500
	PermissionTimeout time.Duration // 0 = reject immediately
}

// Default returns a Config with sensible defaults.
func Default() Config {
	return Config{
		Listen: env("HARNESSD_LISTEN", "127.0.0.1:7800"),
		Token:  os.Getenv("HARNESSD_TOKEN"),
		Agents: AgentsCfg{
			Codex:  AgentCfg{Enabled: true, MinVersion: "0.169.0"},
			Claude: AgentCfg{Enabled: true, MinVersion: "2.1.0"},
		},
		Session: SessionCfg{
			ClaudeIdleTTL: 30 * time.Minute,
			ReplayBufSize: 500,
		},
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
