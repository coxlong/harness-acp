package acp

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	acp "github.com/ironpark/acp-go"
	"github.com/ironpark/acp-go/acp2"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/coxlong/harness-acp/internal/config"
	"github.com/coxlong/harness-acp/internal/registry"
	"github.com/coxlong/harness-acp/internal/rws"
	"github.com/coxlong/harness-acp/internal/session"
	"github.com/coxlong/harness-acp/internal/version"
)

// Server wires the HTTP mux together.
type Server struct {
	mux       *http.ServeMux
	backends  map[string]backend.Backend
	reg       *session.Registry
	cfg       config.Config
	agentReg  *registry.Registry
	endpoints []*rws.Server
}

// NewServer builds a Server and registers all routes.
func NewServer(cfg config.Config, backends map[string]backend.Backend, reg *session.Registry, agentReg *registry.Registry) *Server {
	s := &Server{
		mux:      http.NewServeMux(),
		backends: backends,
		reg:      reg,
		cfg:      cfg,
		agentReg: agentReg,
	}
	s.setup()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Close terminates all active and offline reliable WebSocket connections.
func (s *Server) Close() error {
	for _, ep := range s.endpoints {
		_ = ep.Close()
	}
	return nil
}

// Shutdown closes all reliable WebSocket endpoints and waits for active ACP
// handlers to finish or ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	var firstErr error
	for _, ep := range s.endpoints {
		if err := ep.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Server) setup() {
	// Discovery endpoint — also protected by auth.
	s.mux.Handle("GET /v1/agents", authMiddleware(s.cfg.Token,
		http.HandlerFunc(s.handleDiscover)))

	// One ACP v2 reliable WebSocket endpoint per backend.
	for name, be := range s.backends {
		name := name
		be := be

		agentCfg := s.agentConfig(name)
		reg := s.reg
		if len(s.backends) > 1 {
			// Native IDs and subscriptions belong to their agent endpoint.
			reg = session.New()
		}

		srv := rws.NewServer(
			func(ctx context.Context, t acp.Transport) error {
				conn := acp2.NewAgentSideConnection(
					func(c *acp2.AgentSideConnection) acp2.Agent {
						return NewFacade(name, be, reg, agentCfg.AllowDirs, c)
					},
					t,
				)
				return conn.Start(ctx)
			},
			rws.WithOrigins(s.wsOrigins()...),
			rws.WithDisconnectTTL(s.cfg.Transport.DisconnectTTL),
			rws.WithMaxBufferBytes(s.cfg.Transport.MaxBufferBytes),
			rws.WithMaxOfflineConns(s.cfg.Transport.MaxOfflineConns),
		)
		s.endpoints = append(s.endpoints, srv)

		s.mux.Handle("/acp/"+name, authMiddleware(s.cfg.Token, srv))
		log.Printf("registered ACP endpoint /acp/%s (disconnect TTL=%s)", name, s.cfg.Transport.DisconnectTTL)
	}
}

func (s *Server) agentConfig(name string) config.AgentCfg {
	switch name {
	case "codex":
		return s.cfg.Agents.Codex
	case "claude":
		return s.cfg.Agents.Claude
	}
	return config.AgentCfg{}
}

func (s *Server) wsOrigins() []string {
	if s.cfg.Listen == "127.0.0.1:7800" || s.cfg.Listen == "localhost:7800" {
		return []string{"http://localhost:*", "http://127.0.0.1:*"}
	}
	return []string{"*"}
}

// discoveryResponse is the payload for GET /v1/agents. A client reads it
// before it has an ACP connection, so it carries everything needed to pick an
// endpoint: the daemon version, the ACP revision it speaks, and the agents.
type discoveryResponse struct {
	HarnessdVersion string               `json:"harnessdVersion"`
	ACPVersion      int                  `json:"acpVersion"`
	Agents          []registry.AgentInfo `json:"agents"`
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	agents := s.agentReg.List()
	w.Header().Set("Content-Type", "application/json")
	// A stale agent list would be worse than none: it names endpoints that may
	// no longer exist.
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(discoveryResponse{
		HarnessdVersion: version.Version,
		ACPVersion:      acp2.ProtocolVersion,
		Agents:          agents,
	}); err != nil {
		log.Printf("discovery encode error: %v", err)
	}
}
