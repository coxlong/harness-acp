package registry

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

// AgentInfo holds metadata about a discovered agent. The JSON names are the
// discovery contract (`GET /v1/agents`) the client reads.
type AgentInfo struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Endpoint    string `json:"endpoint"`
	Available   bool   `json:"available"`
	Version     string `json:"version"`
}

// Registry stores known agents, protected by a read-write mutex.
type Registry struct {
	mu     sync.RWMutex
	agents map[string]AgentInfo
}

// New returns an initialised Registry.
func New() *Registry {
	return &Registry{
		agents: make(map[string]AgentInfo),
	}
}

// Probe checks whether name is available on PATH, reads its version, and
// returns an AgentInfo whose Available field is true only when the binary is
// found and its version is >= minVersion (lexicographic / semver-ish).
func (r *Registry) Probe(ctx context.Context, name, displayName, minVersion string) AgentInfo {
	info := AgentInfo{
		Name:        name,
		DisplayName: displayName,
		Endpoint:    "/acp/" + name,
	}

	_, err := exec.LookPath(name)
	if err != nil {
		return info
	}

	cmd := exec.CommandContext(ctx, name, "--version")
	out, err := cmd.Output()
	if err != nil {
		return info
	}

	version := parseSemver(string(out))
	if version == "" {
		return info
	}

	info.Version = version
	if version >= minVersion {
		info.Available = true
	}

	return info
}

// parseSemver scans output for the first token that looks like a semver
// (digits separated by dots, optionally prefixed with 'v').
func parseSemver(output string) string {
	for _, token := range strings.Fields(output) {
		t := strings.TrimPrefix(token, "v")
		parts := strings.Split(t, ".")
		if len(parts) < 2 {
			continue
		}
		valid := true
		for _, p := range parts {
			p = strings.TrimRight(p, "-+abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
			if len(p) == 0 {
				valid = false
				break
			}
			for _, ch := range p {
				if ch < '0' || ch > '9' {
					valid = false
					break
				}
			}
			if !valid {
				break
			}
		}
		if valid {
			return t
		}
	}
	return ""
}

// Register stores or overwrites an agent entry.
func (r *Registry) Register(info AgentInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agents[info.Name] = info
}

// List returns a snapshot of all registered agents, ordered by name so two
// discovery calls cannot disagree on order.
func (r *Registry) List() []AgentInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]AgentInfo, 0, len(r.agents))
	for _, info := range r.agents {
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// ensure fmt is used (avoids import error if callers do not use Sprintf)
var _ = fmt.Sprintf
