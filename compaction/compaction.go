// Package compaction summarises older history so a session can outgrow a model's context window.
package compaction

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/wolfeidau/durable"
)

const (
	defaultReserveTokens = 16384
	defaultKeepTokens    = 20000
	toolResultMaxRunes   = 2000
)

type Config struct {
	Model         durable.Model // summariser: the caller builds it with no OnText
	ContextWindow int64         // 0 disables the threshold; Force still compacts
	ReserveTokens int64         // default 16384
	KeepTokens    int64         // default 20000
}

type Compactor struct{ cfg Config }

var _ durable.Compactor = (*Compactor)(nil)

func New(cfg Config) *Compactor {
	if cfg.ReserveTokens <= 0 {
		cfg.ReserveTokens = defaultReserveTokens
	}
	if cfg.KeepTokens <= 0 {
		cfg.KeepTokens = defaultKeepTokens
	}
	return &Compactor{cfg: cfg}
}

func (c *Compactor) Compact(ctx context.Context, req durable.CompactRequest) (durable.Compaction, bool, error) {
	threshold := c.threshold()
	if !req.Force && (c.cfg.ContextWindow <= 0 || c.estimate(req) <= threshold) {
		return durable.Compaction{}, false, nil
	}
	cut, ok := c.findCut(req.Messages, threshold, req.Force)
	if !ok {
		return durable.Compaction{}, false, nil
	}

	prompt := buildPrompt(req.Previous, req.Messages[:cut])
	resp, err := c.cfg.Model.Generate(ctx, []durable.Message{{Role: durable.RoleUser, Text: prompt}}, nil)
	if err != nil {
		return durable.Compaction{}, false, fmt.Errorf("failed to generate summary: %w", err)
	}
	summary := strings.TrimSpace(resp.Text)
	if summary == "" {
		return durable.Compaction{}, false, errors.New("summariser returned an empty summary")
	}
	return durable.Compaction{Summary: summary, FirstKept: cut}, true, nil
}

// threshold is the token count above which compaction triggers; 0 when there is no window.
func (c *Compactor) threshold() int64 {
	w := c.cfg.ContextWindow
	if w <= 0 {
		return 0
	}
	if c.cfg.ReserveTokens >= w {
		return w / 2
	}
	return w - c.cfg.ReserveTokens
}

func (c *Compactor) estimate(req durable.CompactRequest) int64 {
	msgs := req.Messages
	for i, m := range slices.Backward(msgs) {
		if m.ContextTokens > 0 {
			return msgs[i].ContextTokens + sumTokens(msgs[i+1:])
		}
	}
	return tokens(int64(len(req.Previous))) + sumTokens(msgs)
}

func (c *Compactor) findCut(msgs []durable.Message, threshold int64, force bool) (int, bool) {
	keep := c.cfg.KeepTokens
	if c.cfg.ContextWindow > 0 && threshold/2 < keep {
		keep = threshold / 2
	}

	var acc int64
	for i, m := range slices.Backward(msgs) {
		acc += msgTokens(m)
		if acc >= keep {
			if cut := validCut(msgs, i); cut > 0 {
				return cut, true
			}
			break
		}
	}
	if !force {
		return 0, false
	}
	for i := len(msgs) - 1; i > 0; i-- {
		if msgs[i].Role != durable.RoleTool {
			return i, true
		}
	}
	return 0, false
}

// validCut returns the nearest user message at or after i, else the nearest assistant one, else -1.
func validCut(msgs []durable.Message, i int) int {
	assistant := -1
	for j := i; j < len(msgs); j++ {
		switch msgs[j].Role {
		case durable.RoleUser:
			return j
		case durable.RoleAssistant:
			if assistant < 0 {
				assistant = j
			}
		case durable.RoleTool:
		}
	}
	return assistant
}

func tokens(chars int64) int64 { return (chars + 3) / 4 }

func msgChars(m durable.Message) int64 {
	n := len(m.Text)
	for _, tc := range m.ToolCalls {
		n += len(tc.Name) + len(tc.Input)
	}
	return int64(n)
}

func msgTokens(m durable.Message) int64 { return tokens(msgChars(m)) }

func sumTokens(msgs []durable.Message) int64 {
	var chars int64
	for _, m := range msgs {
		chars += msgChars(m)
	}
	return tokens(chars)
}

func buildPrompt(previous string, msgs []durable.Message) string {
	var b strings.Builder
	b.WriteString("<conversation>\n")
	b.WriteString(serialize(msgs))
	b.WriteString("\n</conversation>\n\n")
	if previous == "" {
		b.WriteString(summarizationPrompt)
		return b.String()
	}
	b.WriteString("<previous-summary>\n")
	b.WriteString(previous)
	b.WriteString("\n</previous-summary>\n\n")
	b.WriteString(updateIntro)
	return b.String()
}

func serialize(msgs []durable.Message) string {
	var parts []string
	for _, m := range msgs {
		switch m.Role {
		case durable.RoleUser:
			if m.Text != "" {
				parts = append(parts, "[User]: "+m.Text)
			}
		case durable.RoleAssistant:
			if m.Text != "" {
				parts = append(parts, "[Assistant]: "+m.Text)
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]string, len(m.ToolCalls))
				for i, tc := range m.ToolCalls {
					calls[i] = tc.Name + "(" + string(tc.Input) + ")"
				}
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
			}
		case durable.RoleTool:
			if m.Text == "" {
				continue
			}
			label := "[Tool result]: "
			if m.IsError {
				label = "[Tool error]: "
			}
			parts = append(parts, label+truncate(m.Text, toolResultMaxRunes))
		}
	}
	return strings.Join(parts, "\n\n")
}

func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return fmt.Sprintf("%s\n\n[... %d more characters truncated]", string(r[:maxRunes]), len(r)-maxRunes)
}
