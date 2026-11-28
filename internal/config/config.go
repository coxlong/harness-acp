package config

import (
	"os"
	"strconv"
	"time"
)

// Config is the top-level harnessd configuration.
type Config struct {
	Listen    string // default "127.0.0.1:7800"
	Token     string // bearer token required in HTTP/WebSocket mode
	TLS       TLSConfig
	Agents    AgentsCfg
	Session   SessionCfg
	Transport TransportCfg
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
	PermissionTimeout time.Duration // 0 = wait while logical connection lives
}

// TransportCfg controls reliable WebSocket connection retention.
type TransportCfg struct {
	DisconnectTTL   time.Duration // how long a logical connection survives offline; default 24h
	MaxBufferBytes  int           // unacked outbound buffer cap per connection; default 64MB
	MaxOfflineConns int           // max offline connections per agent endpoint; default 64
}

// Default returns a Config with sensible defaults.
func Default() Config {
	return Config{
		Listen: env("HARNESSD_LISTEN", "127.0.0.1:7800"),
		Token:  os.Getenv("HARNESSD_TOKEN"),
		Agents: AgentsCfg{
			Codex:  AgentCfg{Enabled: true, MinVersion: "0.171.0"},
			Claude: AgentCfg{Enabled: true, MinVersion: "2.1.0"},
		},
		Session: SessionCfg{
			ClaudeIdleTTL:     30 * time.Minute,
			PermissionTimeout: envDuration("HARNESSD_PERMISSION_TIMEOUT", 0),
		},
		Transport: TransportCfg{
			DisconnectTTL:   envDuration("HARNESSD_DISCONNECT_TTL", 24*time.Hour),
			MaxBufferBytes:  envInt("HARNESSD_MAX_BUFFER_BYTES", 64<<20),
			MaxOfflineConns: envInt("HARNESSD_MAX_OFFLINE_CONNS", 64),
		},
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
