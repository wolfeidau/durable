package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/wolfeidau/durable"
)

// Withheld is the tool result when a PostToolUse hook fails, so the original output never reaches the model.
const Withheld = "The tool ran and may have taken effect, but a hook withheld its output, so the result is unavailable. " +
	"Check the current state before relying on it."

var errWithheld = errors.New(Withheld) //nolint:staticcheck // the text is a sentence for the model

// Options describe the session a Runner serves.
type Options struct {
	SessionID      string
	Workspace      string
	PermissionMode string
	// LogDir receives hooks.log; empty disables the log.
	LogDir string
}

// Runner runs the configured hooks for one session.
type Runner struct {
	cfg  Config
	opts Options
	mu   sync.Mutex // serialises hooks.log writes
}

// New returns a Runner for cfg.
func New(cfg Config, opts Options) *Runner {
	return &Runner{cfg: cfg, opts: opts}
}

type payload struct {
	HookProtocol   int             `json:"hook_protocol"`
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	PermissionMode string          `json:"permission_mode"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolResponse   *toolResponse   `json:"tool_response,omitempty"`
}

type toolResponse struct {
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
}

func (r *Runner) payload(event string, call durable.ToolCall, resp *toolResponse) []byte {
	input := call.Input
	if t := bytes.TrimSpace(input); len(t) == 0 || string(t) == "null" {
		input = json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(payload{
		HookProtocol: 1, SessionID: r.opts.SessionID, CWD: r.opts.Workspace, PermissionMode: r.opts.PermissionMode,
		HookEventName: event, ToolName: call.Name, ToolInput: input, ToolUseID: call.ID, ToolResponse: resp,
	})
	return b
}

func (r *Runner) matching(event, tool string) []Handler {
	var hs []Handler
	for _, g := range r.cfg.events[event] {
		if g.matcher.matches(tool) {
			hs = append(hs, g.hooks...)
		}
	}
	return hs
}

// startsObject reports whether stdout is a JSON document; anything else means no decision.
func startsObject(stdout []byte) ([]byte, bool) {
	s := bytes.TrimSpace(stdout)
	return s, len(s) > 0 && s[0] == '{'
}

type preOutput struct {
	// Decision is Claude Code's older top-level form; ignoring it would turn a deny into no decision.
	Decision           string `json:"decision"`
	HookSpecificOutput *struct {
		PermissionDecision       string          `json:"permissionDecision"`
		PermissionDecisionReason string          `json:"permissionDecisionReason"`
		UpdatedInput             json.RawMessage `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
}

// parsePre returns the decision ("" for none), reason and rewrite (nil for none).
func parsePre(stdout []byte) (string, string, json.RawMessage, error) {
	s, ok := startsObject(stdout)
	if !ok {
		return "", "", nil, nil
	}
	var out preOutput
	if err := json.Unmarshal(s, &out); err != nil {
		return "", "", nil, fmt.Errorf("invalid hook output: %w", err)
	}
	if out.Decision != "" {
		return "", "", nil, fmt.Errorf("unsupported top-level decision %q; use hookSpecificOutput.permissionDecision", out.Decision)
	}
	h := out.HookSpecificOutput
	if h == nil {
		return "", "", nil, nil
	}
	switch h.PermissionDecision {
	case "", "allow", "deny", "ask":
	default:
		return "", "", nil, fmt.Errorf("unsupported permissionDecision %q", h.PermissionDecision)
	}
	var input json.RawMessage
	if h.UpdatedInput != nil {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(h.UpdatedInput, &obj); err != nil || obj == nil {
			return "", "", nil, errors.New("updatedInput must be a JSON object")
		}
		input = h.UpdatedInput
	}
	return h.PermissionDecision, h.PermissionDecisionReason, input, nil
}

// Blocked is the tool result when a PreToolUse hook fails. The cause goes to hooks.log, because a command line can hold secrets.
const Blocked = "A hook failed, so this tool call was blocked and did not run. Do not retry it unchanged; take a different approach."

// evalPre runs one PreToolUse hook. Exit 2 is a deny with stderr as the reason.
func (r *Runner) evalPre(ctx context.Context, call durable.ToolCall, h Handler) (string, string, json.RawMessage, error) {
	res, err := r.invoke(ctx, PreToolUse, call.Name, h, r.payload(PreToolUse, call, nil))
	if err != nil {
		return "", "", nil, err
	}
	if res.exit == 2 {
		return "deny", strings.TrimSpace(res.stderr), nil, nil
	}
	decision, reason, input, err := parsePre(res.stdout)
	if err != nil {
		err = fmt.Errorf("hook %q: %w", h.Command, err)
		r.logFailure(PreToolUse, call.Name, h, err, "")
	}
	return decision, reason, input, err
}

// Gate runs the PreToolUse hooks in config order, then base when no hook decided.
// Failures deny the call rather than returning an error, because an error leaves it pending on every restart.
func (r *Runner) Gate(base durable.ToolGate) durable.ToolGate {
	return func(ctx context.Context, call durable.ToolCall) (durable.Decision, error) {
		var (
			rewrite          json.RawMessage
			anyAsk, anyAllow bool
		)
		for _, h := range r.matching(PreToolUse, call.Name) {
			decision, reason, input, err := r.evalPre(ctx, call, h)
			if ctx.Err() != nil {
				return durable.Decision{}, ctx.Err()
			}
			if err != nil {
				if h.OnFailure == onFailureContinue {
					continue
				}
				return durable.Decision{Action: durable.ActionDeny, Reason: Blocked}, nil
			}

			switch decision {
			case "deny":
				return durable.Decision{Action: durable.ActionDeny, Reason: reason}, nil
			case "ask":
				anyAsk = true
			case "allow":
				anyAllow = true
			}
			if input != nil {
				rewrite = input
				call.Input = input
			}
		}

		switch {
		case anyAsk:
			return durable.Decision{Action: durable.ActionAsk, Input: rewrite}, nil
		case anyAllow:
			return durable.Decision{Action: durable.ActionAllow, Input: rewrite}, nil
		case base == nil:
			return durable.Decision{Action: durable.ActionAllow, Input: rewrite}, nil
		}
		d, err := base(ctx, call)
		if err == nil && d.Input == nil {
			d.Input = rewrite
		}
		return d, err
	}
}

type postOutput struct {
	Decision           string `json:"decision"`
	Reason             string `json:"reason"`
	HookSpecificOutput *struct {
		UpdatedToolOutput json.RawMessage `json:"updatedToolOutput"`
		AdditionalContext string          `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// applyPost returns text updated by a PostToolUse hook's stdout.
func applyPost(text string, stdout []byte) (string, error) {
	s, ok := startsObject(stdout)
	if !ok {
		return text, nil
	}
	var out postOutput
	if err := json.Unmarshal(s, &out); err != nil {
		return "", fmt.Errorf("invalid hook output: %w", err)
	}
	var appended []string
	switch out.Decision {
	case "":
	case "block":
		appended = append(appended, out.Reason)
	default:
		return "", fmt.Errorf("unsupported decision %q", out.Decision)
	}
	if h := out.HookSpecificOutput; h != nil {
		if h.UpdatedToolOutput != nil {
			updated, err := parseUpdatedOutput(h.UpdatedToolOutput)
			if err != nil {
				return "", err
			}
			text = updated
		}
		if h.AdditionalContext != "" {
			appended = append(appended, h.AdditionalContext)
		}
	}
	for _, a := range appended {
		text = appendNote(text, a)
	}
	return text, nil
}

func parseUpdatedOutput(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && raw[0] == '"' {
		return s, nil
	}
	var obj struct {
		Output *string `json:"output"`
	}
	if err := strictDecode(raw, &obj); err != nil || obj.Output == nil || raw[0] != '{' {
		return "", errors.New(`updatedToolOutput must be a string or {"output": "..."}`)
	}
	return *obj.Output, nil
}

func appendNote(text, note string) string {
	if note == "" {
		return text
	}
	if text == "" {
		return note
	}
	return text + "\n" + note
}

type hookTool struct {
	durable.Tool
	r *Runner
}

// Wrap returns tools whose Execute runs the PostToolUse hooks on the result, so the harness commits the updated output.
func (r *Runner) Wrap(tools []durable.Tool) []durable.Tool {
	wrapped := make([]durable.Tool, len(tools))
	for i, t := range tools {
		wrapped[i] = hookTool{Tool: t, r: r}
	}
	return wrapped
}

// Execute runs the tool, then the hooks. Tool errors reach the hooks, and stay errors.
func (t hookTool) Execute(ctx context.Context, call durable.ToolCall) (string, error) {
	out, err := t.Tool.Execute(ctx, call)
	if ctx.Err() != nil {
		return out, err
	}
	hooks := t.r.matching(PostToolUse, call.Name)
	if len(hooks) == 0 {
		return out, err
	}
	isErr := err != nil
	text := out
	if isErr {
		text = err.Error()
	}

	for _, h := range hooks {
		resp := &toolResponse{Output: text, IsError: isErr}
		res, herr := t.r.invoke(ctx, PostToolUse, call.Name, h, t.r.payload(PostToolUse, call, resp))
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if herr == nil && res.exit == 2 {
			text = appendNote(text, strings.TrimSpace(res.stderr))
			continue
		}
		if herr == nil {
			var updated string
			updated, herr = applyPost(text, res.stdout)
			if herr != nil {
				t.r.logFailure(PostToolUse, call.Name, h, herr, "")
			} else {
				text = updated
				continue
			}
		}
		if h.OnFailure == onFailureContinue {
			continue
		}
		return "", errWithheld
	}
	if isErr {
		return "", errors.New(text)
	}
	return text, nil
}

// Recover forwards to the wrapped tool; the embedded interface does not promote optional methods.
func (t hookTool) Recover(ctx context.Context, call durable.ToolCall) string {
	if r, ok := t.Tool.(durable.Recoverer); ok {
		return r.Recover(ctx, call)
	}
	return ""
}

// ReadOnly forwards to the wrapped tool for the same reason as Recover.
func (t hookTool) ReadOnly() bool {
	r, ok := t.Tool.(durable.ReadOnlyTool)
	return ok && r.ReadOnly()
}
