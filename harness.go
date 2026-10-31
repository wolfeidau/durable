package durable

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// UnknownOutcome is the tool result recorded for a non-replayable call that
// was interrupted between starting and recording its result.
const UnknownOutcome = "This tool call was interrupted by a restart before its result was saved. " +
	"It may or may not have taken effect. Check the current state before retrying it."

// Declined is the tool result recorded when Options.Approve rejects a call.
const Declined = "The user declined this tool call, so it did not run. " +
	"Do not retry it unchanged; ask the user or take a different approach."

// Options configures a Harness.
type Options struct {
	Model Model
	Tools []Tool
	// LeaseTTL is how long a crashed process blocks the session before
	// another one may take over. Default 10s (the same as Pi's local agent).
	LeaseTTL time.Duration
	// MaxTurns caps model calls per submission. Default 50.
	MaxTurns int
	// ModelAttempts caps tries per model call for errors marked with Transient;
	// after that Run returns the error and the submission stays running. Default 4.
	ModelAttempts int
	// ModelBackoff is the first retry delay for transient errors, doubling each retry. Default 1s.
	ModelBackoff time.Duration
	// Approve, if set, is asked before a pending tool call starts. Returning
	// false records Declined; an error leaves the call pending for the next Run.
	Approve func(ctx context.Context, call ToolCall) (bool, error)
}

// Harness drives one durable session stored in one SQLite database.
type Harness struct {
	db    *sql.DB
	opts  Options
	tools map[string]Tool
	specs []ToolSpec
	owner string

	leaseLost chan struct{} // closed by renewLoop when the lease is lost
	runSem    chan struct{} // capacity 1; serialises Run

	stopRenew context.CancelFunc
	renewDone chan struct{}
	closeOnce sync.Once
}

// OpenDB opens a session on an already-open database. It waits until it can
// take the session's lease (or ctx ends), so a second process blocks while a
// live one owns the session and takes over once a crashed one's lease expires.
//
// Use Open for the bundled SQLite setup; use OpenDB to bring your own driver
// (e.g. modernc.org/sqlite). Configure the pool with SetMaxOpenConns(1).
func OpenDB(ctx context.Context, db *sql.DB, opts Options) (*Harness, error) {
	if opts.Model == nil {
		return nil, errors.New("durable: Options.Model is required")
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 10 * time.Second
	}
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = 50
	}
	if opts.ModelAttempts <= 0 {
		opts.ModelAttempts = 4
	}
	if opts.ModelBackoff <= 0 {
		opts.ModelBackoff = time.Second
	}
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("durable: migrate: %w", err)
	}

	h := &Harness{db: db, opts: opts, tools: map[string]Tool{}, owner: newOwnerID(),
		leaseLost: make(chan struct{}), runSem: make(chan struct{}, 1)}
	for _, t := range opts.Tools {
		spec := t.Spec()
		h.tools[spec.Name] = t
		h.specs = append(h.specs, spec)
	}

	poll := opts.LeaseTTL / 4
	for {
		ok, err := acquireLease(ctx, db, h.owner, opts.LeaseTTL)
		if err != nil {
			return nil, fmt.Errorf("durable: acquire lease: %w", err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("durable: session is owned by another process: %w", ctx.Err())
		case <-time.After(poll):
		}
	}

	renewCtx, cancel := context.WithCancel(context.Background())
	h.stopRenew = cancel
	h.renewDone = make(chan struct{})
	go h.renewLoop(renewCtx)
	return h, nil
}

func (h *Harness) renewLoop(ctx context.Context) {
	defer close(h.renewDone)
	t := time.NewTicker(h.opts.LeaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// A lost lease is also caught by the fence in every commit;
			// stopping here just avoids pointless writes.
			if errors.Is(renewLease(ctx, h.db, h.owner, h.opts.LeaseTTL), ErrLeaseLost) {
				close(h.leaseLost)
				return
			}
		}
	}
}

// Close releases the lease so another process can open the session at once,
// then closes the database.
func (h *Harness) Close() error {
	var err error
	h.closeOnce.Do(func() {
		h.stopRenew()
		<-h.renewDone
		_ = releaseLease(context.Background(), h.db, h.owner)
		err = h.db.Close()
	})
	return err
}

// Submit records input exactly once. If requestID was seen before, it
// returns that submission (possibly already done) and enqueues nothing.
// It does not run anything; call Run, or use Prompt.
func (h *Harness) Submit(ctx context.Context, requestID, text string) (Submission, error) {
	var sub Submission
	err := h.commit(ctx, func(tx *sql.Tx) error {
		existing, err := getSubmission(ctx, tx, requestID)
		if err == nil {
			sub = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO submissions (request_id, text, status, created_at) VALUES (?, ?, 'queued', ?)`,
			requestID, text, nowMillis())
		if err != nil {
			return err
		}
		seq, _ := res.LastInsertId()
		sub = Submission{Seq: seq, RequestID: requestID, Text: text, Status: StatusQueued}
		return nil
	})
	return sub, err
}

// Get returns a submission by request ID.
func (h *Harness) Get(ctx context.Context, requestID string) (Submission, error) {
	return getSubmission(ctx, h.db, requestID)
}

// Unfinished returns queued and running submissions, oldest first.
func (h *Harness) Unfinished(ctx context.Context) ([]Submission, error) {
	return listUnfinished(ctx, h.db)
}

// History returns the whole transcript across submissions.
func (h *Harness) History(ctx context.Context) ([]Message, error) {
	return loadHistory(ctx, h.db)
}

// Prompt submits input and runs the queue until it is empty, then returns
// the submission's final state.
func (h *Harness) Prompt(ctx context.Context, requestID, text string) (Submission, error) {
	if _, err := h.Submit(ctx, requestID, text); err != nil {
		return Submission{}, err
	}
	if err := h.Run(ctx); err != nil {
		return Submission{}, err
	}
	return h.Get(ctx, requestID)
}

// Run works through unfinished submissions in order until none are left.
// Call it right after Open to resume whatever a crashed process left behind.
//
// It returns early on context cancellation or a lost lease (ErrLeaseLost, which
// also cancels the context passed to tools). A model error not marked Transient
// fails its submission at once and Run moves on. A Transient error is retried
// up to ModelAttempts times, then Run returns it and the submission stays
// running, so an outage pauses work. Nothing is lost on an early return: calling Run again (in this
// process or another) continues from the last commit.
func (h *Harness) Run(ctx context.Context) error {
	select {
	case h.runSem <- struct{}{}:
		defer func() { <-h.runSem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		select {
		case <-h.leaseLost:
			cancel(ErrLeaseLost)
		case <-ctx.Done():
		}
	}()

	err := h.runQueue(ctx)
	if err != nil && ctx.Err() != nil {
		// Surface ErrLeaseLost rather than the context.Canceled it caused.
		return context.Cause(ctx)
	}
	return err
}

func (h *Harness) runQueue(ctx context.Context) error {
	for {
		sub, ok, err := nextUnfinished(ctx, h.db)
		if err != nil || !ok {
			return err
		}
		if err := h.drive(ctx, sub); err != nil {
			return err
		}
	}
}

// drive advances one submission to done or failed.
//
// Each loop iteration reads the state from the database and takes exactly one
// committed step. That is the whole recovery story: after a crash, the
// database holds the last committed step, so the next iteration picks the
// right next step without knowing a crash happened.
func (h *Harness) drive(ctx context.Context, sub Submission) error {
	if sub.Status == StatusQueued {
		err := h.commit(ctx, func(tx *sql.Tx) error {
			if _, err := appendMessage(ctx, tx, sub.Seq, Message{Role: RoleUser, Text: sub.Text}); err != nil {
				return err
			}
			return setSubmission(ctx, tx, sub.Seq, StatusRunning, "", "")
		})
		if err != nil {
			return err
		}
	}

	for {
		calls, hasAssistant, err := lastAssistantCalls(ctx, h.db, sub.Seq)
		if err != nil {
			return err
		}

		// Step: finish outstanding tool calls of the latest assistant message.
		if pending := unfinished(calls); hasAssistant && len(pending) > 0 {
			if err := h.runCall(ctx, sub, pending[0]); err != nil {
				return err
			}
			continue
		}

		// A final assistant message (no tool calls) marks the submission done
		// in the same commit, so reaching here with one means "done" was lost
		// only if the row was edited by hand. Treat it as done anyway.
		if hasAssistant && len(calls) == 0 {
			return h.commit(ctx, func(tx *sql.Tx) error {
				return setSubmission(ctx, tx, sub.Seq, StatusDone, "", "")
			})
		}

		// Step: call the model.
		turns, err := h.assistantTurns(ctx, sub.Seq)
		if err != nil {
			return err
		}
		if turns >= h.opts.MaxTurns {
			return h.commit(ctx, func(tx *sql.Tx) error {
				return setSubmission(ctx, tx, sub.Seq, StatusFailed, "",
					fmt.Sprintf("stopped after %d model turns", turns))
			})
		}
		if err := h.modelStep(ctx, sub); err != nil {
			return err
		}
		var status string
		if err := h.db.QueryRowContext(ctx, `SELECT status FROM submissions WHERE seq = ?`, sub.Seq).Scan(&status); err != nil {
			return err
		}
		if s := SubmissionStatus(status); s == StatusDone || s == StatusFailed {
			return nil
		}
	}
}

func unfinished(calls []storedCall) []storedCall {
	var out []storedCall
	for _, c := range calls {
		if c.Status != callDone {
			out = append(out, c)
		}
	}
	return out
}

func (h *Harness) assistantTurns(ctx context.Context, submissionSeq int64) (int, error) {
	var n int
	err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE submission_seq = ? AND role = 'assistant'`,
		submissionSeq).Scan(&n)
	return n, err
}

// modelStep calls the model and commits its reply together with the intent of
// every tool call it asked for. If the process dies during the call, nothing
// was committed and the next attempt calls the model again.
func (h *Harness) modelStep(ctx context.Context, sub Submission) error {
	history, err := loadHistory(ctx, h.db)
	if err != nil {
		return err
	}
	var reply Message
	backoff := h.opts.ModelBackoff
	for attempt := 1; ; attempt++ {
		var err error
		reply, err = h.opts.Model.Generate(ctx, history, h.specs)
		if err == nil {
			break
		}
		// Cancellation is the process going away, never a model failure.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isTransient(err) {
			return h.commit(ctx, func(tx *sql.Tx) error {
				return setSubmission(ctx, tx, sub.Seq, StatusFailed, "",
					fmt.Sprintf("model failed: %v", err))
			})
		}
		if attempt >= h.opts.ModelAttempts {
			return fmt.Errorf("failed to call model after %d attempts: %w", attempt, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	reply.Role = RoleAssistant

	return h.commit(ctx, func(tx *sql.Tx) error {
		msgSeq, err := appendMessage(ctx, tx, sub.Seq, reply)
		if err != nil {
			return err
		}
		for i, c := range reply.ToolCalls {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO tool_calls (call_id, message_seq, position, name, input, status)
				 VALUES (?, ?, ?, ?, ?, 'pending')`,
				c.ID, msgSeq, i, c.Name, string(c.Input)); err != nil {
				return fmt.Errorf("durable: record tool call %q: %w", c.ID, err)
			}
		}
		if len(reply.ToolCalls) == 0 {
			return setSubmission(ctx, tx, sub.Seq, StatusDone, reply.Text, "")
		}
		return nil
	})
}

// runCall takes one tool call one committed step forward:
//
//	pending -> commit "started" -> execute -> commit result + tool message
//	started (crash happened mid-call) -> replay if safe, else record UnknownOutcome
func (h *Harness) runCall(ctx context.Context, sub Submission, c storedCall) error {
	tool, known := h.tools[c.Name]

	switch {
	case !known:
		return h.finishCall(ctx, sub, c, fmt.Sprintf("unknown tool %q", c.Name), true)
	case c.Status == callStarted && tool.Replay() != ReplaySafe:
		return h.finishCall(ctx, sub, c, UnknownOutcome, true)
	case c.Status == callPending:
		if h.opts.Approve != nil {
			ok, err := h.opts.Approve(ctx, c.ToolCall)
			if err != nil {
				return fmt.Errorf("failed to approve tool call %q: %w", c.ID, err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !ok {
				return h.finishCall(ctx, sub, c, Declined, true)
			}
		}
		err := h.commit(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`UPDATE tool_calls SET status = 'started' WHERE message_seq = ? AND position = ?`,
				c.MessageSeq, c.Position)
			return err
		})
		if err != nil {
			return err
		}
	}

	out, err := tool.Execute(ctx, c.ToolCall)
	if ctx.Err() != nil {
		// Treat cancellation as the process going away: leave the call
		// "started" so the next run applies the replay rule.
		return ctx.Err()
	}
	isErr := false
	if err != nil {
		out, isErr = err.Error(), true
	}
	return h.finishCall(ctx, sub, c, out, isErr)
}

func (h *Harness) finishCall(ctx context.Context, sub Submission, c storedCall, out string, isErr bool) error {
	return h.commit(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tool_calls SET status = 'done', result = ?, is_error = ? WHERE message_seq = ? AND position = ?`,
			out, isErr, c.MessageSeq, c.Position); err != nil {
			return err
		}
		_, err := appendMessage(ctx, tx, sub.Seq, Message{
			Role: RoleTool, ToolCallID: c.ID, Text: out, IsError: isErr,
		})
		return err
	})
}

func newOwnerID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
