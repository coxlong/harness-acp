package acp

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	acp "github.com/ironpark/acp-go"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/acphttp"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/coxlong/harness-acp/internal/config"
	"github.com/coxlong/harness-acp/internal/registry"
	"github.com/coxlong/harness-acp/internal/session"
)

// Server wires the HTTP mux together.
type Server struct {
	mux      *http.ServeMux
	backends map[string]backend.Backend
	reg      *session.Registry
	cfg      config.Config
	agentReg *registry.Registry
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

func (s *Server) setup() {
	// Discovery endpoint — also protected by auth.
	s.mux.Handle("GET /v1/agents", authMiddleware(s.cfg.Token,
		http.HandlerFunc(s.handleDiscover)))

	// One ACP v2 endpoint per backend.
	for name, be := range s.backends {
		name := name // capture
		be := be

		agentCfg := s.agentConfig(name)
		reg := s.reg

		srv := acphttp.NewServer(
			func(ctx context.Context, t acp.Transport) error {
				conn := acp2.NewAgentSideConnection(
					func(c *acp2.AgentSideConnection) acp2.Agent {
						return NewFacade(name, be, reg, agentCfg.AllowDirs, c)
					},
					t,
				)
				return conn.Start(ctx)
			},
			acphttp.WithWebSocketOrigins(s.wsOrigins()...),
			acphttp.WithWebSocketPing(15*time.Second),
			acphttp.WithIdleTimeout(5*time.Minute),
		)

		s.mux.Handle("/acp/"+name, authMiddleware(s.cfg.Token, srv))
		log.Printf("registered ACP endpoint /acp/%s", name)
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

// discoveryResponse is the payload for GET /v1/agents.
type discoveryResponse struct {
	HarnessdVersion string               `json:"harnessdVersion"`
	ACPVersion      int                  `json:"acpVersion"`
	Agents          []registry.AgentInfo `json:"agents"`
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	agents := s.agentReg.List()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(discoveryResponse{
		HarnessdVersion: "0.1.0",
		ACPVersion:      2,
		Agents:          agents,
	}); err != nil {
		log.Printf("discovery encode error: %v", err)
	}
}
