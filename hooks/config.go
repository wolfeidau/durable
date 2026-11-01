// Package hooks runs user-configured subprocess hooks around tool calls, using
// Claude Code's JSON contract. PreToolUse is a durable.ToolGate and PostToolUse
// wraps durable.Tool, so the harness needs no changes.
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

const (
	// PreToolUse runs before a tool call and may deny, ask or rewrite it.
	PreToolUse = "PreToolUse"
	// PostToolUse runs after a tool call and may replace its output.
	PostToolUse = "PostToolUse"

	onFailureBlock    = "block"
	onFailureContinue = "continue"

	defaultTimeoutSeconds = 60
)

// Handler is one command to run for an event.
type Handler struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// Timeout is in seconds; zero uses the default.
	Timeout int `json:"timeout,omitempty"`
	// OnFailure is "block" (default) or "continue".
	OnFailure string `json:"onFailure,omitempty"`
	// UseWorkspaceCwd runs the hook in the workspace instead of an empty temp directory.
	UseWorkspaceCwd bool `json:"useWorkspaceCwd,omitempty"`
}

type group struct {
	matcher matcher
	hooks   []Handler
}

// Config holds the hooks per event, in config order.
type Config struct {
	events map[string][]group
}

// Empty reports whether no hooks are configured.
func (c Config) Empty() bool {
	return len(c.Counts()) == 0
}

// Counts returns the number of handlers per event, omitting events with none.
func (c Config) Counts() map[string]int {
	counts := map[string]int{}
	for event, groups := range c.events {
		for _, g := range groups {
			counts[event] += len(g.hooks)
		}
	}
	for event, n := range counts {
		if n == 0 {
			delete(counts, event)
		}
	}
	return counts
}

// UnknownMatchers warns about each matcher entry that matches none of tools.
// A name copied from Claude Code, such as Bash, would otherwise never fire and quietly skip a policy.
func (c Config) UnknownMatchers(tools []string) []string {
	var warnings []string
	for _, event := range []string{PreToolUse, PostToolUse} {
		for _, g := range c.events[event] {
			for _, name := range g.matcher.names {
				if !slices.Contains(tools, name) {
					warnings = append(warnings, fmt.Sprintf("%s matcher %q matches no tool", event, name))
				}
			}
			if re := g.matcher.re; re != nil && !slices.ContainsFunc(tools, re.MatchString) {
				warnings = append(warnings, fmt.Sprintf("%s matcher %q matches no tool in this session", event, re))
			}
		}
	}
	return warnings
}

type rawGroup struct {
	Matcher string            `json:"matcher"`
	Hooks   []json.RawMessage `json:"hooks"`
}

// LoadConfig reads the hooks from path. A missing file means no hooks. An
// unsupported event is a warning. Anything else that cannot be honoured is an
// error, because silently skipping a safety hook is worse than not starting.
func LoadConfig(path string) (Config, []string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the caller's trusted config file
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil, nil
	}
	if err != nil {
		return Config{}, nil, fmt.Errorf("failed to read hooks config: %w", err)
	}
	var file struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	// Strict, because a misspelt key would otherwise load no hooks and report nothing.
	if err := strictDecode(data, &file); err != nil {
		return Config{}, nil, fmt.Errorf("failed to parse hooks config %s: %w", path, err)
	}

	events := make([]string, 0, len(file.Hooks))
	for event := range file.Hooks {
		events = append(events, event)
	}
	slices.Sort(events)

	cfg := Config{events: map[string][]group{}}
	var warnings []string
	for _, event := range events {
		if event != PreToolUse && event != PostToolUse {
			// A case slip is a misspelt safety hook, not a copied event we don't support.
			if strings.EqualFold(event, PreToolUse) || strings.EqualFold(event, PostToolUse) {
				return Config{}, nil, fmt.Errorf("hook event %q in %s is misspelt", event, path)
			}
			warnings = append(warnings, fmt.Sprintf("hook event %q is not supported and was ignored", event))
			continue
		}
		var raws []rawGroup
		if err := strictDecode(file.Hooks[event], &raws); err != nil {
			return Config{}, nil, fmt.Errorf("failed to parse %s hooks in %s: %w", event, path, err)
		}
		for i, rg := range raws {
			g, err := buildGroup(rg)
			if err != nil {
				return Config{}, nil, fmt.Errorf("%s hooks[%d] in %s: %w", event, i, path, err)
			}
			cfg.events[event] = append(cfg.events[event], g)
		}
	}
	return cfg, warnings, nil
}

func buildGroup(rg rawGroup) (group, error) {
	m, err := parseMatcher(rg.Matcher)
	if err != nil {
		return group{}, err
	}
	g := group{matcher: m}
	for i, raw := range rg.Hooks {
		var h Handler
		if err := strictDecode(raw, &h); err != nil {
			return group{}, fmt.Errorf("hooks[%d]: %w", i, err)
		}
		if h.Type != "command" {
			return group{}, fmt.Errorf("hooks[%d]: type %q is not supported, only \"command\"", i, h.Type)
		}
		if h.Command == "" {
			return group{}, fmt.Errorf("hooks[%d]: command is required", i)
		}
		if h.Timeout < 0 {
			return group{}, fmt.Errorf("hooks[%d]: timeout must not be negative", i)
		}
		switch h.OnFailure {
		case "":
			h.OnFailure = onFailureBlock
		case onFailureBlock, onFailureContinue:
		default:
			return group{}, fmt.Errorf("hooks[%d]: onFailure %q must be %q or %q", i, h.OnFailure, onFailureBlock, onFailureContinue)
		}
		g.hooks = append(g.hooks, h)
	}
	return g, nil
}

func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after JSON value")
	}
	return nil
}

type matcher struct {
	all   bool
	names []string
	re    *regexp.Regexp
}

// listChars are the characters of an exact name or list; anything else is a regexp.
var listChars = regexp.MustCompile(`^[A-Za-z0-9_\-,| ]*$`)

func parseMatcher(s string) (matcher, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "*" {
		return matcher{all: true}, nil
	}
	if listChars.MatchString(s) {
		var names []string
		for _, n := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '|' }) {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
		return matcher{names: names}, nil
	}
	re, err := regexp.Compile(s)
	if err != nil {
		return matcher{}, fmt.Errorf("invalid matcher %q: %w", s, err)
	}
	return matcher{re: re}, nil
}

func (m matcher) matches(tool string) bool {
	switch {
	case m.all:
		return true
	case m.re != nil:
		return m.re.MatchString(tool)
	}
	return slices.Contains(m.names, tool)
}
