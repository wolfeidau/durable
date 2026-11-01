// Package responses implements durable.Model on top of the OpenAI Responses API.
package responses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/rawjson"
)

const (
	minOutputTokens = 16
	maxErrorBody    = 4096
	noToolOutput    = "(no tool output)"
)

var defaultSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// Config configures the adapter.
type Config struct {
	// BaseURL includes the version, e.g. https://openrouter.ai/api/v1.
	BaseURL string
	APIKey  string
	Model   string
	System  string
	// MaxTokens is sent as max_output_tokens only when set.
	MaxTokens int64
	// Effort is sent as reasoning.effort only when set.
	Effort string
	// SessionID is sent as x-session-id so a gateway keeps the session on one upstream.
	SessionID string
	// OnText receives streamed text deltas. A retried stream may repeat text.
	OnText func(string)
	// HTTPClient defaults to a client without a timeout, because streams are long-lived.
	HTTPClient *http.Client
}

// Model is a durable.Model backed by the Responses API.
type Model struct {
	cfg    Config
	client *http.Client
}

var _ durable.Model = (*Model)(nil)

// New returns a Model using cfg.
func New(cfg Config) *Model {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Model{cfg: cfg, client: client}
}

type request struct {
	Model           string     `json:"model"`
	Instructions    string     `json:"instructions,omitempty"`
	Input           []any      `json:"input"`
	Tools           []tool     `json:"tools,omitempty"`
	Store           bool       `json:"store"`
	Stream          bool       `json:"stream"`
	MaxOutputTokens int64      `json:"max_output_tokens,omitempty"`
	Reasoning       *reasoning `json:"reasoning,omitempty"`
	Include         []string   `json:"include,omitempty"`
}

type reasoning struct {
	Effort string `json:"effort"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Generate sends history and tools to the API and returns the assistant turn.
func (m *Model) Generate(ctx context.Context, history []durable.Message, tools []durable.ToolSpec) (durable.Message, error) {
	input, err := toInput(history)
	if err != nil {
		return durable.Message{}, err
	}
	req := request{
		Model:        m.cfg.Model,
		Instructions: m.cfg.System,
		Input:        input,
		Tools:        toTools(tools),
	}
	if m.cfg.MaxTokens > 0 {
		req.MaxOutputTokens = max(m.cfg.MaxTokens, minOutputTokens)
	}
	if m.cfg.Effort != "" {
		req.Reasoning = &reasoning{Effort: m.cfg.Effort}
		req.Include = []string{"reasoning.encrypted_content"}
	}
	req.Stream = true
	// HTML escaping would rewrite <, > and & inside replayed items.
	body, err := rawjson.Marshal(req)
	if err != nil {
		return durable.Message{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return durable.Message{}, fmt.Errorf("failed to create request: %w", m.redact(err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	if m.cfg.SessionID != "" {
		httpReq.Header.Set("x-session-id", m.cfg.SessionID)
	}

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return durable.Message{}, m.classify(ctx, fmt.Errorf("failed to send request: %w", m.redact(err)))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		err := fmt.Errorf("responses API returned status %d: %s", resp.StatusCode, m.redactText(strings.TrimSpace(string(b))))
		if s := resp.StatusCode; s == 408 || s == 409 || s == 429 || s >= 500 {
			return durable.Message{}, durable.Transient(err)
		}
		return durable.Message{}, err
	}
	return m.readStream(ctx, resp.Body)
}

func (m *Model) redactText(s string) string {
	if m.cfg.APIKey == "" {
		return s
	}
	return strings.ReplaceAll(s, m.cfg.APIKey, "[redacted]")
}

func (m *Model) redact(err error) error {
	if m.cfg.APIKey == "" || !strings.Contains(err.Error(), m.cfg.APIKey) {
		return err
	}
	return errors.New(m.redactText(err.Error()))
}

func (m *Model) classify(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return durable.Transient(err)
}

type streamEvent struct {
	Type        string          `json:"type"`
	OutputIndex *int            `json:"output_index"`
	Delta       json.RawMessage `json:"delta"`
	Item        json.RawMessage `json:"item"`
	Response    *responseBody   `json:"response"`
	Code        any             `json:"code"`
	Message     string          `json:"message"`
	Error       json.RawMessage `json:"error"`
}

type responseBody struct {
	Output            []json.RawMessage `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
}

type item struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

type doneItem struct {
	index *int
	raw   json.RawMessage
}

type streamState struct {
	m       *Model
	done    []doneItem
	pending map[string]bool
}

func (m *Model) readStream(ctx context.Context, body io.Reader) (durable.Message, error) {
	st := &streamState{m: m, pending: map[string]bool{}}
	var data []string
	br := bufio.NewReader(body)

	// dispatch returns a result once a terminal event arrives.
	dispatch := func() (*durable.Message, error) {
		if len(data) == 0 {
			return nil, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			return nil, nil
		}
		return st.handle(payload)
	}

	for {
		line, readErr := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if msg, err := dispatch(); err != nil || msg != nil {
				return deref(msg, err)
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if readErr != nil {
			if msg, err := dispatch(); err != nil || msg != nil {
				return deref(msg, err)
			}
			return durable.Message{}, m.endOfStream(ctx, readErr)
		}
	}
}

func (m *Model) endOfStream(ctx context.Context, readErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !errors.Is(readErr, io.EOF) {
		return m.classify(ctx, fmt.Errorf("failed to read stream: %w", m.redact(readErr)))
	}
	return durable.Transient(errors.New("stream ended before a terminal response event"))
}

func (st *streamState) handle(payload string) (*durable.Message, error) {
	var ev streamEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil, durable.Transient(fmt.Errorf("failed to decode stream event: %w", err))
	}
	switch ev.Type {
	case "response.output_text.delta":
		var s string
		if st.m.cfg.OnText != nil && json.Unmarshal(ev.Delta, &s) == nil {
			st.m.cfg.OnText(s)
		}
	case "response.output_item.added":
		var it item
		if json.Unmarshal(ev.Item, &it) == nil && it.Type == "function_call" {
			st.pending[itemKey(it)] = true
		}
	case "response.output_item.done":
		var it item
		if err := json.Unmarshal(ev.Item, &it); err != nil {
			return nil, fmt.Errorf("failed to decode output item: %w", err)
		}
		// An upstream may only fill in id on done, so clear both possible keys.
		delete(st.pending, it.ID)
		delete(st.pending, it.CallID)
		st.done = append(st.done, doneItem{index: ev.OutputIndex, raw: ev.Item})
	case "response.completed":
		return st.complete(ev)
	case "response.incomplete":
		reason := "no reason given"
		if ev.Response != nil && ev.Response.IncompleteDetails != nil && ev.Response.IncompleteDetails.Reason != "" {
			reason = ev.Response.IncompleteDetails.Reason
		}
		return nil, fmt.Errorf("response incomplete: %s", reason)
	case "response.failed":
		var e apiError
		if ev.Response != nil && ev.Response.Error != nil {
			e = *ev.Response.Error
		}
		return nil, st.m.classifyStream(e.Code, e.Message)
	case "error":
		e := apiError{Code: ev.Code, Message: ev.Message}
		var nested apiError
		if json.Unmarshal(ev.Error, &nested) == nil && (nested.Code != nil || nested.Message != "") {
			e = nested
		}
		return nil, st.m.classifyStream(e.Code, e.Message)
	}
	return nil, nil
}

func (st *streamState) complete(ev streamEvent) (*durable.Message, error) {
	if len(st.pending) > 0 {
		return nil, errors.New("response completed with a tool call that never finished")
	}
	var out []json.RawMessage
	if ev.Response != nil {
		out = ev.Response.Output
	}
	items, err := collect(st.done, out)
	if err != nil {
		return nil, err
	}
	msg, err := toResult(items)
	if err != nil {
		return nil, err
	}
	return &msg, nil
}

func deref(msg *durable.Message, err error) (durable.Message, error) {
	if err != nil {
		return durable.Message{}, err
	}
	return *msg, nil
}

func itemKey(it item) string {
	if it.CallID != "" {
		return it.CallID
	}
	return it.ID
}

// collect orders streamed items and backfills reasoning content the stream left out.
func collect(done []doneItem, final []json.RawMessage) ([]json.RawMessage, error) {
	if len(done) == 0 {
		return final, nil
	}
	for _, d := range done {
		if d.index == nil {
			return nil, errors.New("response items have no output_index, so their order is unknown")
		}
	}
	sort.SliceStable(done, func(i, j int) bool { return *done[i].index < *done[j].index })

	encrypted := map[string]string{}
	for _, r := range final {
		var it item
		if json.Unmarshal(r, &it) == nil && it.Type == "reasoning" && it.ID != "" && it.EncryptedContent != "" {
			encrypted[it.ID] = it.EncryptedContent
		}
	}

	items := make([]json.RawMessage, len(done))
	for i, d := range done {
		items[i] = d.raw
		var it item
		if json.Unmarshal(d.raw, &it) != nil || it.Type != "reasoning" || it.EncryptedContent != "" {
			continue
		}
		enc, ok := encrypted[it.ID]
		if !ok {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(d.raw, &fields); err != nil {
			return nil, fmt.Errorf("failed to decode reasoning item: %w", err)
		}
		fields["encrypted_content"], _ = rawjson.Marshal(enc)
		b, err := rawjson.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("failed to encode reasoning item: %w", err)
		}
		items[i] = b
	}
	return items, nil
}

func toResult(items []json.RawMessage) (durable.Message, error) {
	var text strings.Builder
	var calls []durable.ToolCall
	raw := make([]json.RawMessage, 0, len(items))
	for _, r := range items {
		raw = append(raw, r)
		var it item
		if err := json.Unmarshal(r, &it); err != nil {
			return durable.Message{}, fmt.Errorf("failed to decode output item: %w", err)
		}
		switch it.Type {
		case "message":
			for _, part := range it.Content {
				switch part.Type {
				case "output_text":
					text.WriteString(part.Text)
				case "refusal":
					return durable.Message{}, fmt.Errorf("model refused to respond: %s", part.Refusal)
				}
			}
		case "function_call":
			input, err := toolInput(it.Arguments)
			if err != nil {
				return durable.Message{}, fmt.Errorf("tool call %q has invalid arguments: %w", it.Name, err)
			}
			calls = append(calls, durable.ToolCall{ID: it.CallID, Name: it.Name, Input: input})
		}
	}
	rawJSON, err := rawjson.Marshal(raw)
	if err != nil {
		return durable.Message{}, fmt.Errorf("failed to marshal assistant content: %w", err)
	}
	return durable.Message{Role: durable.RoleAssistant, Text: text.String(), ToolCalls: calls, Raw: rawJSON}, nil
}

// toolInput is committed as-is, so only a JSON object may pass.
func toolInput(args string) (json.RawMessage, error) {
	args = strings.TrimSpace(args)
	if args == "" || args == "null" {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err != nil || obj == nil {
		return nil, errors.New("arguments are not a JSON object")
	}
	return json.RawMessage(args), nil
}

func (m *Model) classifyStream(code any, message string) error {
	c := ""
	if code != nil {
		c = fmt.Sprint(code)
	}
	err := fmt.Errorf("responses API error (code %q): %s", c, m.redactText(message))
	switch c {
	case "server_error", "rate_limit_exceeded", "rate_limit_error", "overloaded", "overloaded_error", "api_error", "timeout", "service_unavailable", "408", "409", "429", "500", "502", "503", "504", "529":
		return durable.Transient(err)
	}
	return err
}

func toTools(specs []durable.ToolSpec) []tool {
	out := make([]tool, 0, len(specs))
	for _, s := range specs {
		schema := s.Schema
		if len(schema) == 0 {
			schema = defaultSchema
		}
		out = append(out, tool{Type: "function", Name: s.Name, Description: s.Description, Parameters: schema})
	}
	return out
}

func toInput(history []durable.Message) ([]any, error) {
	out := []any{}
	var results, texts []any

	// Tool results go before a following prompt, as the API expects them next to their calls.
	flush := func() {
		out = append(append(out, results...), texts...)
		results, texts = nil, nil
	}

	for _, msg := range history {
		switch msg.Role {
		case durable.RoleUser:
			texts = append(texts, map[string]any{"type": "message", "role": "user", "content": msg.Text})
		case durable.RoleTool:
			text := msg.Text
			if text == "" {
				text = noToolOutput
			}
			if msg.IsError {
				text = "Error: " + text
			}
			results = append(results, map[string]any{"type": "function_call_output", "call_id": msg.ToolCallID, "output": text})
		case durable.RoleAssistant:
			flush()
			items, err := assistantItems(msg)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
		default:
			return nil, fmt.Errorf("unknown message role %q", msg.Role)
		}
	}
	flush()
	return out, nil
}

func assistantItems(msg durable.Message) ([]any, error) {
	if len(msg.Raw) > 0 {
		var raws []json.RawMessage
		if err := json.Unmarshal(msg.Raw, &raws); err != nil {
			return nil, fmt.Errorf("failed to decode raw assistant content: %w", err)
		}
		items := make([]any, len(raws))
		for i, r := range raws {
			items[i] = r
		}
		return items, nil
	}
	// Item ids are left out because OpenAI validates them by prefix.
	var items []any
	if msg.Text != "" {
		items = append(items, map[string]any{
			"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": msg.Text, "annotations": []any{}}},
		})
	}
	for _, c := range msg.ToolCalls {
		args := string(c.Input)
		if strings.TrimSpace(args) == "" || args == "null" {
			args = "{}"
		}
		items = append(items, map[string]any{"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": args})
	}
	return items, nil
}
