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

	"github.com/wolfeidau/durable"
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

type repl struct {
	h     *durable.Harness
	lines <-chan string
	sigs  <-chan os.Signal
	out   io.Writer
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
		fmt.Fprintln(r.out, "\ncancelled")
	case res.err != nil:
		fmt.Fprintf(r.out, "\nerror: %v\n", res.err)
	case res.sub.Status == durable.StatusFailed:
		fmt.Fprintf(r.out, "\nfailed: %s\n", res.sub.Error)
	default:
		fmt.Fprintln(r.out)
	}
	return false, nil
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
			return nil
		case <-r.sigs:
			fmt.Fprintln(r.out)
			return nil
		case l, ok := <-r.lines:
			if !ok {
				fmt.Fprintln(r.out)
				return nil
			}
			line = l
		}
		if strings.TrimSpace(line) == "" {
			continue
		}

		id := randomID()
		if stop, err := r.work(ctx, func(c context.Context) (durable.Submission, error) { return r.h.Prompt(c, id, line) }); stop {
			return err
		}
	}
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
				fmt.Fprintf(r.out, "\nfailed: %s\n", cur.Error)
			}
		}
	}
	return r.finish(ctx, res)
}

func randomID() string {
	return rand.Text()
}
