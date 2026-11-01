// Package mcp exposes tools from MCP servers as durable.Tool. It is separate
// from package tools so that importing tools does not link the MCP SDK.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
)

const (
	typeStdio = "stdio"
	typeHTTP  = "http"
)

// Server is one entry of the Claude Code mcpServers config.
type Server struct {
	Type           string            `json:"type,omitempty"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
}

var envVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// LoadConfig reads the mcpServers map from path. A missing file means no servers.
// Entries that cannot be used are skipped with a warning. The returned Type is
// always "stdio" or "http".
func LoadConfig(path string) (map[string]Server, []string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the user's config flag
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read MCP config: %w", err)
	}
	var cfg struct {
		MCPServers map[string]Server `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("failed to parse MCP config %s: %w", path, err)
	}

	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	slices.Sort(names)

	servers := map[string]Server{}
	var warnings []string
	for _, name := range names {
		s := cfg.MCPServers[name]
		switch {
		case s.Type == "" && s.Command != "":
			s.Type = typeStdio
		case s.Type == "" && s.URL != "":
			s.Type = typeHTTP
		case s.Type == "":
			warnings = append(warnings, fmt.Sprintf("mcp server %q skipped: it has neither command nor url", name))
			continue
		case s.Type != typeStdio && s.Type != typeHTTP:
			warnings = append(warnings, fmt.Sprintf("mcp server %q skipped: type %q is not supported", name, s.Type))
			continue
		}
		if s.Type == typeStdio && s.Command == "" || s.Type == typeHTTP && s.URL == "" {
			warnings = append(warnings, fmt.Sprintf("mcp server %q skipped: type %q needs a command or url", name, s.Type))
			continue
		}
		if s.Env, err = expand(name, s.Env); err != nil {
			return nil, nil, err
		}
		if s.Headers, err = expand(name, s.Headers); err != nil {
			return nil, nil, err
		}
		servers[name] = s
	}
	return servers, warnings, nil
}

func expand(server string, m map[string]string) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		var missing string
		out[k] = envVar.ReplaceAllStringFunc(v, func(ref string) string {
			name := ref[2 : len(ref)-1]
			val, ok := os.LookupEnv(name)
			if !ok && missing == "" {
				missing = name
			}
			return val
		})
		if missing != "" {
			return nil, fmt.Errorf("mcp server %q: %q references unset environment variable %s", server, k, missing)
		}
	}
	return out, nil
}
