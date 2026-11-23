package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	acppkg "github.com/coxlong/harness-acp/internal/acp"
	"github.com/coxlong/harness-acp/internal/backend"
	claudebe "github.com/coxlong/harness-acp/internal/backend/claude"
	codexbe "github.com/coxlong/harness-acp/internal/backend/codex"
	"github.com/coxlong/harness-acp/internal/config"
	"github.com/coxlong/harness-acp/internal/registry"
	"github.com/coxlong/harness-acp/internal/session"
	"github.com/coxlong/harness-acp/internal/version"

	acp "github.com/ironpark/acp-go"
	"github.com/ironpark/acp-go/acp2"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	stdioMode := flag.Bool("stdio", false, "run in stdio mode (single agent)")
	agentName := flag.String("agent", "claude", "agent to use in --stdio mode")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	cfg := config.Default()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, *stdioMode, *agentName); err != nil {
		log.Fatalf("harnessd: %v", err)
	}
}

func run(ctx context.Context, cfg config.Config, stdioMode bool, agentName string) error {
	agentReg := registry.New()
	backends := make(map[string]backend.Backend)
	sess := session.New(cfg.Session.ReplayBufSize)

	if cfg.Agents.Codex.Enabled && (!stdioMode || agentName == "codex") {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		info := agentReg.Probe(probeCtx, "codex", "Codex", cfg.Agents.Codex.MinVersion)
		if info.Available {
			be := codexbe.New(ctx, "codex", cfg.Agents.Codex.AllowDirs)
			_, err := be.Probe(probeCtx)
			if err == nil {
				backends["codex"] = be
				defer be.Close()
				log.Printf("codex backend ready (v%s)", info.Version)
			} else {
				_ = be.Close()
				info.Available = false
				log.Printf("codex app-server unavailable: %v", err)
			}
		} else {
			log.Printf("codex not available (not found or version too old)")
		}
		cancel()
		agentReg.Register(info)
	}

	if cfg.Agents.Claude.Enabled && (!stdioMode || agentName == "claude") {
		info := agentReg.Probe(ctx, "claude", "Claude", cfg.Agents.Claude.MinVersion)
		agentReg.Register(info)
		if info.Available {
			rt := claudebe.NewRuntime(ctx, claudebe.RuntimeConfig{
				IdleTTL:  cfg.Session.ClaudeIdleTTL,
				MaxProcs: 20,
			})
			be := claudebe.NewBackend(rt)
			backends["claude"] = be
			defer be.Close()
			log.Printf("claude backend ready (v%s)", info.Version)
		} else {
			log.Printf("claude not available (not found or version too old)")
		}
	}

	if stdioMode {
		return runStdio(ctx, cfg, backends, sess, agentName)
	}
	return runServe(ctx, cfg, backends, sess, agentReg)
}

func runServe(ctx context.Context, cfg config.Config, backends map[string]backend.Backend, sess *session.Registry, agentReg *registry.Registry) error {
	srv := acppkg.NewServer(cfg, backends, sess, agentReg)
	httpSrv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      srv,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming
	}
	log.Printf("harnessd listening on %s", cfg.Listen)
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

func runStdio(ctx context.Context, cfg config.Config, backends map[string]backend.Backend, sess *session.Registry, agentName string) error {
	be, ok := backends[agentName]
	if !ok {
		return fmt.Errorf("agent %q not available", agentName)
	}
	log.Printf("harnessd stdio mode, agent=%s", agentName)
	agentCfg := agentConfigFor(cfg, agentName)
	t := acp.NewStdioTransport(os.Stdin, os.Stdout)
	conn := acp2.NewAgentSideConnection(
		func(c *acp2.AgentSideConnection) acp2.Agent {
			return acppkg.NewFacade(agentName, be, sess, agentCfg.AllowDirs, c)
		},
		t,
		acp.WithErrorHandler(func(err error) {
			log.Printf("stdio connection error: %v", err)
		}),
	)
	return conn.Start(ctx)
}

func agentConfigFor(cfg config.Config, name string) config.AgentCfg {
	switch name {
	case "codex":
		return cfg.Agents.Codex
	case "claude":
		return cfg.Agents.Claude
	}
	return config.AgentCfg{}
}
