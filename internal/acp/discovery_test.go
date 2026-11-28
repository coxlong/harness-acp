package acp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ironpark/acp-go/acp2"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/coxlong/harness-acp/internal/config"
	"github.com/coxlong/harness-acp/internal/registry"
	"github.com/coxlong/harness-acp/internal/session"
	"github.com/coxlong/harness-acp/internal/version"
)

const testBearerToken = "test-secret"

// discoveryServer builds a server with no ACP endpoints: discovery is the only
// route under test.
func discoveryServer(t *testing.T, cfg config.Config, agents ...registry.AgentInfo) http.Handler {
	t.Helper()
	reg := registry.New()
	for _, info := range agents {
		reg.Register(info)
	}
	return NewServer(cfg, map[string]backend.Backend{}, session.New(), reg)
}

func getDiscovery(t *testing.T, h http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The client reads these exact field names; Go's default names would be
// PascalCase and silently decode to zero values.
func TestDiscoveryUsesLowerCamelFields(t *testing.T) {
	cfg := config.Default()
	cfg.Token = testBearerToken
	h := discoveryServer(t, cfg,
		registry.AgentInfo{Name: "claude", DisplayName: "Claude", Endpoint: "/acp/claude", Available: true, Version: "2.1.328"},
	)

	rec := getDiscovery(t, h, testBearerToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"harnessdVersion", "acpVersion", "agents"} {
		if _, ok := body[key]; !ok {
			t.Errorf("missing %q in %s", key, rec.Body.String())
		}
	}
	if body["harnessdVersion"] != version.Version {
		t.Errorf("harnessdVersion = %v, want %s", body["harnessdVersion"], version.Version)
	}
	if body["acpVersion"] != float64(acp2.ProtocolVersion) {
		t.Errorf("acpVersion = %v, want %d", body["acpVersion"], acp2.ProtocolVersion)
	}

	agents, _ := body["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("agents = %v, want one", body["agents"])
	}
	agent, _ := agents[0].(map[string]any)
	for key, want := range map[string]any{
		"name":        "claude",
		"displayName": "Claude",
		"endpoint":    "/acp/claude",
		"available":   true,
		"version":     "2.1.328",
	} {
		if agent[key] != want {
			t.Errorf("agents[0].%s = %v, want %v", key, agent[key], want)
		}
	}
}

// Discovery order must not depend on map iteration; a client that remembers the
// first available agent would otherwise pick a different one per request.
func TestDiscoveryOrdersAgentsByName(t *testing.T) {
	cfg := config.Default()
	cfg.Token = testBearerToken
	h := discoveryServer(t, cfg,
		registry.AgentInfo{Name: "codex", DisplayName: "Codex", Endpoint: "/acp/codex"},
		registry.AgentInfo{Name: "claude", DisplayName: "Claude", Endpoint: "/acp/claude"},
		registry.AgentInfo{Name: "amp", DisplayName: "Amp", Endpoint: "/acp/amp"},
	)

	var got discoveryResponse
	rec := getDiscovery(t, h, testBearerToken)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Agents) != 3 || got.Agents[0].Name != "amp" || got.Agents[2].Name != "codex" {
		t.Fatalf("agents = %v, want amp, claude, codex", namesOf(got.Agents))
	}
}

func namesOf(agents []registry.AgentInfo) []string {
	out := make([]string, len(agents))
	for i, agent := range agents {
		out[i] = agent.Name
	}
	return out
}

// A rejecting server has to say it is an auth problem: the client turns this
// into "update the token", not "server offline".
func TestDiscoveryRejectsWithoutBearer(t *testing.T) {
	cfg := config.Default()
	cfg.Token = "s3cret"
	h := discoveryServer(t, cfg)

	rec := getDiscovery(t, h, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="harnessd"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if body["error"] != "unauthorized" {
		t.Errorf("error = %v, want unauthorized", body["error"])
	}

	if rec := getDiscovery(t, h, "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token status = %d, want 401", rec.Code)
	}
	if rec := getDiscovery(t, h, "s3cret"); rec.Code != http.StatusOK {
		t.Errorf("right token status = %d, want 200", rec.Code)
	}
}

func TestDiscoveryRejectsBlankConfiguredToken(t *testing.T) {
	for _, token := range []string{"", " \t"} {
		cfg := config.Default()
		cfg.Token = token
		h := discoveryServer(t, cfg)

		if rec := getDiscovery(t, h, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("configured token %q: status = %d, want 401", token, rec.Code)
		}
	}
}
