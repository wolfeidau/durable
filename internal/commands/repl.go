package commands

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/skills"
)

// readLines feeds stdin lines to one channel shared by the prompt and approval.
func readLines(r io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				ch <- strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

const skillPrefix = "/skill:"

type repl struct {
	h      *durable.Harness
	skills []skills.Skill
	lines  <-chan string
	sigs   <-chan os.Signal
	out    io.Writer
	// piped reports turn failures in the exit status, because a script cannot read the transcript.
	piped      bool
	failed     bool  // a submission failed; later turns do not undo that
	unfinished error // the last turn stopped early; a later turn can still finish the work
}

type turnResult struct {
	sub        durable.Submission
	err        error
	terminated bool // SIGTERM arrived during the turn
	forced     bool // a second signal arrived before the turn stopped
}

// turn runs fn under a context that the first signal cancels.
func (r *repl) turn(ctx context.Context, fn func(context.Context) (durable.Submission, error)) turnResult {
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan turnResult, 1)
	go func() {
		sub, err := fn(tctx)
		done <- turnResult{sub: sub, err: err}
	}()

	var signalled, terminated bool
	for {
		select {
		case res := <-done:
			res.terminated = terminated
			return res
		case s := <-r.sigs:
			if signalled {
				return turnResult{forced: true}
			}
			signalled = true
			terminated = s == syscall.SIGTERM
			cancel()
		}
	}
}

// finish reports a turn's outcome and says whether the REPL should stop.
func (r *repl) finish(ctx context.Context, res turnResult) (bool, error) {
	switch {
	case res.forced:
		if err := r.h.Detach(); err != nil {
			return true, fmt.Errorf("failed to detach from session: %w", err)
		}
		return true, errors.New("interrupted twice, exiting without waiting for the turn")
	case errors.Is(res.err, durable.ErrLeaseLost):
		return true, fmt.Errorf("session lease lost: %w", res.err)
	case res.terminated || ctx.Err() != nil:
		return true, nil //nolint:nilerr // a stopping process has no one to report the turn's error to
	case errors.Is(res.err, context.Canceled):
		if err := r.cancelUnfinished(ctx); err != nil {
			return true, fmt.Errorf("failed to cancel the task: %w", err)
		}
		r.unfinished = nil
		fmt.Fprintln(r.out, "\ncancelled")
	case res.err != nil:
		r.unfinished = res.err
		fmt.Fprintf(r.out, "\nerror: %v\n", res.err)
	case res.sub.Status == durable.StatusFailed:
		r.failed = true
		r.unfinished = nil
		fmt.Fprintf(r.out, "\nfailed: %s\n", res.sub.Error)
	default:
		r.unfinished = nil
		fmt.Fprintln(r.out)
	}
	return false, nil
}

// exitErr is what run returns when input ends, so piped callers see failed turns.
func (r *repl) exitErr() error {
	switch {
	case !r.piped:
		return nil
	case r.unfinished != nil:
		return fmt.Errorf("input ended before the turn finished: %w", r.unfinished)
	case r.failed:
		return errors.New("one or more submissions failed")
	}
	return nil
}

func (r *repl) cancelUnfinished(ctx context.Context) error {
	subs, err := r.h.Unfinished(ctx)
	if err != nil {
		return err
	}
	for _, s := range subs {
		if _, err := r.h.Cancel(ctx, s.RequestID); err != nil {
			return err
		}
	}
	return nil
}

func (r *repl) run(ctx context.Context) error {
	// Resume unfinished work before the first prompt.
	if stop, err := r.work(ctx, func(c context.Context) (durable.Submission, error) { return durable.Submission{}, r.h.Run(c) }); stop {
		return err
	}

	for {
		fmt.Fprint(r.out, "> ")
		var line string
		select {
		case <-ctx.Done():
			return r.exitErr()
		case <-r.sigs:
			fmt.Fprintln(r.out)
			return r.exitErr()
		case l, ok := <-r.lines:
			if !ok {
				fmt.Fprintln(r.out)
				return r.exitErr()
			}
			line = l
		}
		if strings.TrimSpace(line) == "" {
			continue
		}

		prompt, ok := r.expandSkill(line)
		if !ok {
			continue
		}
		id := randomID()
		if stop, err := r.work(ctx, func(c context.Context) (durable.Submission, error) { return r.h.Prompt(c, id, prompt) }); stop {
			return err
		}
	}
}

// expandSkill turns a /skill:<name> [text] line into the prompt to submit; other lines pass through.
// ok is false when the line was rejected and nothing should be submitted.
func (r *repl) expandSkill(line string) (prompt string, ok bool) {
	rest, found := strings.CutPrefix(line, skillPrefix)
	if !found {
		return line, true
	}
	name, text := rest, ""
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		name, text = rest[:i], strings.TrimSpace(rest[i:])
	}
	s, known := skills.Find(r.skills, name)
	if name == "" || !known {
		fmt.Fprintf(r.out, "unknown skill %q; the skills in this session are listed when it starts, or start a new --session to reload them\n", name)
		return "", false
	}
	expanded, err := skills.Expand(s, text)
	if err != nil {
		fmt.Fprintf(r.out, "error: failed to load skill %q: %v\n", name, err)
		return "", false
	}
	return expanded, true
}

// work runs a turn and reports earlier submissions it resumed that failed, which no caller waits on.
func (r *repl) work(ctx context.Context, fn func(context.Context) (durable.Submission, error)) (bool, error) {
	resumed, err := r.h.Unfinished(ctx)
	if err != nil {
		return true, err
	}
	res := r.turn(ctx, fn)
	if !res.forced {
		for _, s := range resumed {
			if cur, err := r.h.Get(ctx, s.RequestID); err == nil && cur.Status == durable.StatusFailed {
				r.failed = true
				fmt.Fprintf(r.out, "\nfailed: %s\n", cur.Error)
			}
		}
	}
	return r.finish(ctx, res)
}

func randomID() string {
	return rand.Text()
}
