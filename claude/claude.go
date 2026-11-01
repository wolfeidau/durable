// Package claude implements durable.Model on top of the Anthropic Messages API.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/param"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/rawjson"
)

// With tools defined the summarizer can call one instead of writing a summary.
const compactInstructions = "Summarize the transcript inside <summary></summary> tags. " +
	"Include relevant information in the summary for continuing the task in the next context window. " +
	"Do not call any tools while writing this summary; respond with text only."

const (
	// DefaultModel is used when Config.Model is empty.
	DefaultModel     = "claude-sonnet-5-5"
	defaultMaxTokens = 64000
	defaultEffort    = "medium"
)

// olderThanClaude5 matches models where adaptive thinking is rejected or would turn thinking on.
// An optional vendor prefix covers gateway names like anthropic/claude-sonnet-4.5.
var olderThanClaude5 = regexp.MustCompile(`^([a-z0-9-]+/)?claude-([a-z]+-)?[1-4]([-.]|$)`)

// Config configures the adapter. Zero values select the defaults.
type Config struct {
	Model     string
	System    string
	MaxTokens int64
	Effort    string
	// Gateway drops the Anthropic-only betas, fallbacks, compaction and thinking block binding, which gateways may reject.
	Gateway bool
	// OnText receives streamed text deltas. A retried stream may repeat text.
	OnText func(string)
	// OnCompact is called when the API starts summarizing older history into a compaction block.
	OnCompact func()
}

// Model is a durable.Model backed by Claude.
type Model struct {
	client anthropic.Client
	cfg    Config
}

var _ durable.Model = (*Model)(nil)

// New returns a Model using client and cfg.
func New(client anthropic.Client, cfg Config) *Model {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = defaultMaxTokens
	}
	if cfg.Effort == "" {
		cfg.Effort = defaultEffort
	}
	return &Model{client: client, cfg: cfg}
}

// Generate sends history and tools to the API and returns the assistant turn.
func (m *Model) Generate(ctx context.Context, history []durable.Message, tools []durable.ToolSpec) (durable.Message, error) {
	msgs, err := toMessages(history)
	if err != nil {
		return durable.Message{}, err
	}

	params := anthropic.BetaMessageNewParams{
		Model:     m.cfg.Model,
		MaxTokens: m.cfg.MaxTokens,
		Messages:  msgs,
		Tools:     toTools(tools),
		// Top-level cache control caches tools, system and history together.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(m.cfg.Effort)},
	}
	if !m.cfg.Gateway {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
	}
	if !olderThanClaude5.MatchString(m.cfg.Model) {
		adaptive := &anthropic.BetaThinkingConfigAdaptiveParam{}
		if !m.cfg.Gateway {
			// Stored thinking is bound to the system prompt and tools, so a new binary must degrade rather than fail the session.
			params.Betas = append(params.Betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01, anthropic.AnthropicBetaCompact2026_01_12)
			params.ContextManagement = anthropic.BetaContextManagementConfigParam{
				Edits: []anthropic.BetaContextManagementConfigEditUnionParam{{
					OfCompact20260112: &anthropic.BetaCompact20260112EditParam{Instructions: anthropic.String(compactInstructions)},
				}},
			}
			adaptive.BlockBinding = anthropic.BetaThinkingBlockBindingParam{
				PrefixMismatchBehavior: anthropic.BetaThinkingPrefixMismatchBehaviorDropBlock,
			}
		}
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: adaptive}
	}
	if m.cfg.System != "" {
		params.System = []anthropic.BetaTextBlockParam{{Text: m.cfg.System}}
	}

	stream := m.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	var acc anthropic.BetaMessage
	for stream.Next() {
		ev := stream.Current()
		if err := acc.Accumulate(ev); err != nil {
			return durable.Message{}, durable.Transient(fmt.Errorf("failed to accumulate stream event: %w", err))
		}
		if m.cfg.OnCompact != nil && ev.Type == "content_block_start" && ev.ContentBlock.Type == "compaction" {
			m.cfg.OnCompact()
		}
		if m.cfg.OnText != nil && ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
			m.cfg.OnText(ev.Delta.Text)
		}
	}
	if err := stream.Err(); err != nil {
		return durable.Message{}, classify(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return durable.Message{}, err
	}

	return toResult(acc)
}

func toResult(acc anthropic.BetaMessage) (durable.Message, error) {
	switch acc.StopReason {
	case anthropic.BetaStopReasonEndTurn, anthropic.BetaStopReasonToolUse, anthropic.BetaStopReasonStopSequence:
	case "":
		// The stream ended without message_delta, so the connection was cut.
		return durable.Message{}, durable.Transient(errors.New("stream ended before a stop reason"))
	case anthropic.BetaStopReasonRefusal:
		return durable.Message{}, fmt.Errorf("model refused to respond (category %q): %s", acc.StopDetails.Category, acc.StopDetails.Explanation)
	default:
		return durable.Message{}, fmt.Errorf("unexpected stop reason %q", acc.StopReason)
	}

	var text strings.Builder
	var calls []durable.ToolCall
	raw := make([]json.RawMessage, 0, len(acc.Content))
	for _, block := range acc.Content {
		raw = append(raw, json.RawMessage(block.RawJSON()))
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			calls = append(calls, durable.ToolCall{ID: block.ID, Name: block.Name, Input: append(json.RawMessage(nil), block.Input...)})
		}
	}
	// HTML escaping would rewrite <, > and & inside signed thinking text.
	rawJSON, err := rawjson.Marshal(raw)
	if err != nil {
		return durable.Message{}, fmt.Errorf("failed to marshal assistant content: %w", err)
	}
	return durable.Message{Role: durable.RoleAssistant, Text: text.String(), ToolCalls: calls, Raw: rawJSON}, nil
}

func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	wrapped := fmt.Errorf("failed to generate: %w", err)
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return durable.Transient(wrapped)
	}
	// An SSE error event carries the 200 status of its stream, so use the error type.
	if apiErr.StatusCode < 400 {
		switch apiErr.Type() {
		case anthropic.ErrorTypeOverloadedError, anthropic.ErrorTypeRateLimitError, anthropic.ErrorTypeAPIError, anthropic.ErrorTypeTimeoutError:
			return durable.Transient(wrapped)
		default:
		}
		return wrapped
	}
	if s := apiErr.StatusCode; s == 408 || s == 409 || s == 429 || s >= 500 {
		return durable.Transient(wrapped)
	}
	return wrapped
}

func toTools(specs []durable.ToolSpec) []anthropic.BetaToolUnionParam {
	out := make([]anthropic.BetaToolUnionParam, 0, len(specs))
	for _, s := range specs {
		schema := anthropic.BetaToolInputSchemaParam{Properties: map[string]any{}}
		if len(s.Schema) > 0 {
			schema = param.Override[anthropic.BetaToolInputSchemaParam](s.Schema)
		}
		t := &anthropic.BetaToolParam{
			Name:        s.Name,
			Description: anthropic.String(s.Description),
			InputSchema: schema,
		}
		if !s.NonStrict {
			t.Strict = anthropic.Bool(true)
		}
		out = append(out, anthropic.BetaToolUnionParam{OfTool: t})
	}
	return out
}

func toMessages(history []durable.Message) ([]anthropic.BetaMessageParam, error) {
	var out []anthropic.BetaMessageParam
	var results, texts []anthropic.BetaContentBlockParamUnion

	// Parallel tool results and a prompt after them must share one user turn.
	flush := func() {
		if len(results)+len(texts) == 0 {
			return
		}
		blocks := append(append([]anthropic.BetaContentBlockParamUnion{}, results...), texts...)
		out = append(out, anthropic.NewBetaUserMessage(blocks...))
		results, texts = nil, nil
	}

	for _, msg := range history {
		switch msg.Role {
		case durable.RoleUser:
			texts = append(texts, anthropic.NewBetaTextBlock(msg.Text))
		case durable.RoleTool:
			results = append(results, anthropic.NewBetaToolResultBlock(msg.ToolCallID, msg.Text, msg.IsError))
		case durable.RoleAssistant:
			flush()
			blocks, err := assistantBlocks(msg)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{Content: blocks}})
		default:
			return nil, fmt.Errorf("unknown message role %q", msg.Role)
		}
	}
	flush()
	return out, nil
}

func assistantBlocks(msg durable.Message) ([]anthropic.BetaContentBlockParamUnion, error) {
	if len(msg.Raw) == 0 {
		var blocks []anthropic.BetaContentBlockParamUnion
		if msg.Text != "" {
			blocks = append(blocks, anthropic.NewBetaTextBlock(msg.Text))
		}
		for _, c := range msg.ToolCalls {
			blocks = append(blocks, anthropic.NewBetaToolUseBlock(c.ID, c.Input, c.Name))
		}
		return blocks, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(msg.Raw, &raws); err != nil {
		return nil, fmt.Errorf("failed to decode raw assistant content: %w", err)
	}
	blocks := make([]anthropic.BetaContentBlockParamUnion, len(raws))
	for i, r := range raws {
		blocks[i] = param.Override[anthropic.BetaContentBlockParamUnion](r)
	}
	return blocks, nil
}
