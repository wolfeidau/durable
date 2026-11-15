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

	"github.com/wolfeidau/durable/internal/userfile"
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
	// UseWorkspaceCwd runs a stdio server in the workspace instead of an empty temp directory.
	UseWorkspaceCwd bool `json:"useWorkspaceCwd,omitempty"`
}

// UnmarshalJSON accepts unquoted numbers and booleans in args, env and headers, as YAML users write them.
func (s *Server) UnmarshalJSON(data []byte) error {
	type plain Server
	aux := struct {
		*plain
		Args    json.RawMessage `json:"args"`
		Env     json.RawMessage `json:"env"`
		Headers json.RawMessage `json:"headers"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	var err error
	if s.Args, err = userfile.StringList("args", aux.Args); err != nil {
		return err
	}
	if s.Env, err = userfile.StringMap("env", aux.Env); err != nil {
		return err
	}
	s.Headers, err = userfile.StringMap("headers", aux.Headers)
	return err
}

var envVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ParseConfig reads the mcpServers map from JSON data, which is nil when there is no file.
// Entries that cannot be used are skipped with a warning. A server that
// references an unset variable is skipped too, and the error names it while the
// other servers are still returned. The returned Type is always "stdio" or "http".
func ParseConfig(data []byte, label string) (map[string]Server, []string, error) {
	if data == nil {
		return nil, nil, nil
	}
	var cfg struct {
		MCPServers map[string]Server `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("failed to parse MCP config %s: %w", label, err)
	}

	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	slices.Sort(names)

	servers := map[string]Server{}
	var warnings []string
	var errs []error
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
		var envErr, headerErr error
		s.Env, envErr = expand(name, s.Env)
		s.Headers, headerErr = expand(name, s.Headers)
		if err := errors.Join(envErr, headerErr); err != nil {
			errs = append(errs, err)
			continue
		}
		servers[name] = s
	}
	return servers, warnings, errors.Join(errs...)
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
