// Package durable is a small durable agent harness: every step of an agent
// run is committed to SQLite before the next step starts, so a crashed or
// killed process can be reopened and the run continues where it stopped.
//
// The design follows the same ideas as Pi Durable:
//
//   - The database is the source of truth. There is no separate "recovery"
//     code path: the run loop always reads the current state from storage
//     and decides the next step from it, so starting fresh and resuming
//     after a crash are the same code.
//   - Input is exactly-once. Each submission carries a request ID; submitting
//     the same ID twice returns the first submission instead of starting new work.
//   - Tool calls are recorded as intent -> effect -> result. A call that was
//     started but never recorded a result has an unknown outcome, and is only
//     re-executed if the tool declares it safe to replay.
//   - One writer per session. A lease row in the database fences every
//     commit, so a stale process can't write after another one took over.
package durable

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
)

// Role is who produced a message in the transcript.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a model's request to run a tool. ID comes from the model
// provider and is stable once committed, so tools can use it as an
// idempotency key for external side effects.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Message is one transcript entry. Assistant messages may carry ToolCalls;
// tool messages carry the result for ToolCallID.
type Message struct {
	Role       Role       `json:"role"`
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
	// Raw is provider-specific content replayed verbatim to the same provider.
	Raw json.RawMessage `json:"raw,omitempty"`
	// ContextTokens is the provider-reported context size after this reply; 0 means unknown.
	ContextTokens int64 `json:"context_tokens,omitempty"`
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	// The schema comes from outside (e.g. an MCP server) and may not meet strict tool rules.
	NonStrict bool `json:"non_strict,omitempty"`
}

// Model is your provider adapter. It must not have side effects beyond the
// API call itself: if the process dies mid-call the harness simply calls it
// again with the same history.
type Model interface {
	Generate(ctx context.Context, history []Message, tools []ToolSpec) (Message, error)
}

type transientError struct{ err error }

func (e transientError) Error() string { return e.err.Error() }
func (e transientError) Unwrap() error { return e.err }

// Transient marks a model error as worth retrying (rate limits, outages,
// network faults). Unmarked errors fail the submission at once.
func Transient(err error) error {
	if err == nil {
		return nil
	}
	return transientError{err}
}

func isTransient(err error) bool {
	var t transientError
	return errors.As(err, &t)
}

type overflowError struct{ err error }

func (e overflowError) Error() string { return e.err.Error() }
func (e overflowError) Unwrap() error { return e.err }

// ContextOverflow marks a model error as the request exceeding the context window,
// which a Compactor may fix. Unmarked errors fail the submission at once.
func ContextOverflow(err error) error {
	if err == nil {
		return nil
	}
	return overflowError{err}
}

func isOverflow(err error) bool {
	var o overflowError
	return errors.As(err, &o)
}

var overflowExclusions = compilePatterns(
	`^(?:Throttling error|Service unavailable):`,
	`rate limit`,
	`too many requests`,
)

var overflowPatterns = compilePatterns(
	`prompt (?:is )?too long`,
	`request_too_large`,
	`input is too long for requested model`,
	`exceeds the context window`,
	`exceeds (?:the )?(?:model'?s )?maximum context length`,
	`context window exceeded`,
	`input token count.*exceeds the maximum`,
	`maximum prompt length is \d+`,
	`reduce the length of the messages`,
	`maximum context length is \d+ tokens`,
	`exceeds (?:the )?maximum allowed input length of [\d,]+ tokens?`,
	`too large for model with \d+ maximum context length`,
	`context[_ ]length[_ ]exceeded`,
	`too many tokens`,
	`token limit exceeded`,
)

func compilePatterns(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(`(?i)` + p)
	}
	return out
}

// IsOverflowText reports whether a provider error message says the request exceeded the context window.
func IsOverflowText(s string) bool {
	for _, re := range overflowExclusions {
		if re.MatchString(s) {
			return false
		}
	}
	for _, re := range overflowPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// Compactor shrinks the context the model sees. Options.Compactor is optional.
type Compactor interface {
	// Compact returns ok=false when nothing should change.
	Compact(ctx context.Context, req CompactRequest) (Compaction, bool, error)
}

// CompactRequest is the context the model currently sees.
type CompactRequest struct {
	Previous string    // summary from the last compaction
	Messages []Message // messages after that summary, not including it
	Force    bool      // the provider reported overflow, so skip any threshold
}

// Compaction replaces the messages before FirstKept with Summary in what the model sees.
type Compaction struct {
	Summary   string
	FirstKept int // index into CompactRequest.Messages; 0 < FirstKept < len(Messages)
}

// Replay says what to do with a tool call that started before a crash but
// never recorded its result.
type Replay int

const (
	// ReplayNever: the outcome is unknown, so the harness records an error
	// result telling the model to check the current state before retrying.
	// Use this for anything with side effects: bash, file writes, HTTP POSTs.
	ReplayNever Replay = iota
	// ReplaySafe: running the call again is harmless (reads, searches, or
	// writes that are idempotent by construction).
	ReplaySafe
)

// Tool is something the model can call.
type Tool interface {
	Spec() ToolSpec
	Replay() Replay
	// Execute runs the call. A returned error becomes an error result the
	// model sees; it does not stop the run. Context cancellation does stop
	// the run without recording a result (the same as the process dying).
	// Tools must honour ctx: it is also cancelled when the lease is lost, and
	// a tool that ignores it keeps running alongside the new owner.
	Execute(ctx context.Context, call ToolCall) (string, error)
}

// Recoverer is an optional Tool extension for calls whose process outlived a crash.
type Recoverer interface {
	// Recover stops anything left from a started call and returns text to add
	// to its result. It must be idempotent: a crash can happen after it runs
	// but before the result commits, so it may run again for the same call.
	Recover(ctx context.Context, call ToolCall) string
}

// SubmissionStatus is where a submission is in its lifecycle.
type SubmissionStatus string

const (
	StatusQueued  SubmissionStatus = "queued"
	StatusRunning SubmissionStatus = "running"
	StatusDone    SubmissionStatus = "done"
	StatusFailed  SubmissionStatus = "failed"
	// StatusCancelled is terminal, like done and failed; see Harness.Cancel.
	StatusCancelled SubmissionStatus = "cancelled"
)

// Submission is one unit of user input and the run it triggered.
type Submission struct {
	Seq       int64
	RequestID string
	Text      string
	Status    SubmissionStatus
	Result    string // final assistant text when Status is done
	Error     string // reason when Status is failed
}
