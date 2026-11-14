package config

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/wolfeidau/durable/internal/commands"
)

// describeRef names where a secret comes from without printing the reference, which can name a vault and item.
func describeRef(ref string) string {
	switch {
	case strings.HasPrefix(ref, "op://"):
		return "1Password"
	case ref == "gh":
		return "gh"
	case strings.HasPrefix(ref, "env:"):
		return "env " + strings.TrimPrefix(ref, "env:")
	}
	return "unset"
}

// Entry is one effective setting for display. Value never holds a secret reference.
type Entry struct {
	Field  string
	Value  string
	Source Source
}

// Entries returns each effective setting with its source in display order. It never resolves a secret.
func (s *Settings) Entries() []Entry {
	var out []Entry
	line := func(field, value string) {
		out = append(out, Entry{Field: field, Value: value, Source: s.Sources[field]})
	}
	orDefault := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	line("listen.addr", s.Addr)
	line("listen.idle_timeout", s.IdleTimeout.String())
	line("listen.open_timeout", s.OpenTimeout.String())
	line("listen.heartbeat", s.Heartbeat.String())
	line("provider.name", orDefault(s.Provider, "anthropic"))
	line("provider.protocol", orDefault(s.Protocol, "provider default"))
	line("provider.model", orDefault(s.Model, "built-in default"))
	switch {
	case s.APIKey != "":
		line("provider.api_key", describeRef(s.APIKey))
	case s.Sources["provider.api_key"] == SourceFlag:
		line("provider.api_key", "set by flag")
	case s.Provider == "openrouter":
		line("provider.api_key", "env OPENROUTER_API_KEY")
	default:
		line("provider.api_key", "provider default credentials")
	}
	line("git.author", s.GitAuthor)
	for _, kind := range slices.Sorted(maps.Keys(s.GitHosts)) {
		line("git.hosts."+kind+".token", describeRef(s.GitHosts[kind]))
	}
	for _, name := range slices.Sorted(maps.Keys(s.Environments)) {
		e := s.Environments[name]
		value := "image=" + e.Image
		if len(e.Grants) > 0 {
			value += " grants=" + strings.Join(e.Grants, ",")
		}
		if e.Approvals == commands.ApprovalsAuto {
			value += " approvals=auto"
		}
		line("environments."+name, value)
	}
	return out
}

// Describe writes each effective setting with its source.
func (s *Settings) Describe(w io.Writer) {
	for _, e := range s.Entries() {
		fmt.Fprintf(w, "%-34s %s  [%s]\n", e.Field, e.Value, e.Source)
	}
	if len(s.Environments) == 0 {
		fmt.Fprintln(w, "environments                       none")
	}
}
