package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/commands"
)

// blockingModel blocks its first call until the context ends and answers later ones.
type blockingModel struct {
	calls   atomic.Int32
	started chan struct{}
}

func (m *blockingModel) Generate(ctx context.Context, _ []durable.Message, _ []durable.ToolSpec) (durable.Message, error) {
	if m.calls.Add(1) == 1 {
		close(m.started)
		<-ctx.Done()
		return durable.Message{}, ctx.Err()
	}
	return durable.Message{Role: durable.RoleAssistant, Text: "hi"}, nil
}

func TestCancelSubmissionInterruptsRunningSubmission(t *testing.T) {
	e := newEnv(t)
	// A long backoff proves the queue continues because of the kick, not the retry.
	e.cfg.RetryBase = time.Hour
	model := &blockingModel{started: make(chan struct{})}
	e.cfg.assemble = func(ctx context.Context, c commands.AssembleConfig) (*commands.Assembled, error) {
		c.APIToken = "k"
		a, err := commands.Assemble(ctx, c)
		if err == nil {
			a.Options.Model = model
			a.Options.ModelAttempts = 1
		}
		return a, err
	}
	r := e.registry(t)
	s, release, err := r.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release()

	_, err = s.Harness().Submit(t.Context(), "r1", "first")
	require.NoError(t, err)
	s.Kick()
	<-model.started
	_, err = s.Harness().Submit(t.Context(), "r2", "second")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sub, err := s.CancelSubmission(ctx, "r1")
	require.NoError(t, err)
	require.Equal(t, durable.StatusCancelled, sub.Status)

	require.Eventually(t, func() bool {
		got, err := s.Harness().Get(t.Context(), "r2")
		return err == nil && got.Status == durable.StatusDone
	}, 10*time.Second, 10*time.Millisecond)
}
