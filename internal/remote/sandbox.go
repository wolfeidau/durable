package remote

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"

	"github.com/wolfeidau/durable"
)

// SandboxName is the OpenShell name for a session: names allow 19 lowercase characters, and a session ID is 16 hex.
func SandboxName(sessionID string) string { return "ds-" + sessionID }

// Sandboxes gives each session its own sandbox. Spike only: worker tokens live in memory, so a sandbox that
// outlives the server can't be reached again.
type Sandboxes struct {
	l      *SDKLauncher
	image  string
	logger *slog.Logger

	mu     sync.Mutex
	tokens map[string]string
}

// NewSandboxes returns a manager that creates sandboxes from image.
func NewSandboxes(l *SDKLauncher, image string, logger *slog.Logger) *Sandboxes {
	return &Sandboxes{l: l, image: image, logger: logger, tokens: map[string]string{}}
}

// Tools makes sure the session's sandbox is running and returns remote tools plus a func that stops it.
func (m *Sandboxes) Tools(ctx context.Context, sessionID string) ([]durable.Tool, func(), error) {
	name := SandboxName(sessionID)
	token, err := m.ensure(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	c := m.l.Dial(name, token, WorkerPort)
	stop := func() {
		c.CloseIdle()
		// The session's context is gone by now, and Stop must still finish.
		sctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := m.l.Stop(sctx, name); err != nil {
			m.logger.Error("failed to stop sandbox", slog.String("sandbox", name), slog.Any("error", err))
		}
	}
	// The worker may not be listening yet when the sandbox first reports ready.
	var ts []durable.Tool
	deadline := time.Now().Add(30 * time.Second)
	for {
		ts, err = c.Tools(ctx)
		if err == nil || ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		stop()
		return nil, nil, err
	}
	return ts, stop, nil
}

func (m *Sandboxes) ensure(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token, known := m.tokens[name]
	sb, err := m.l.c.Sandboxes().Get(ctx, Workspace, name)
	switch {
	case err == nil:
		if !known {
			return "", fmt.Errorf("sandbox %s exists but this server has no worker token for it; run durable-server sandbox-delete", name)
		}
		switch sb.Status.Phase {
		case v1.SandboxReady:
		case v1.SandboxStopped:
			if err := m.l.Start(ctx, name); err != nil {
				return "", err
			}
		default:
			if _, err := m.l.c.Sandboxes().WaitReady(ctx, Workspace, name); err != nil {
				return "", fmt.Errorf("failed to wait for sandbox %s in phase %v: %w", name, sb.Status.Phase, err)
			}
		}
		return token, nil
	case v1.IsNotFound(err):
		token = rand.Text()
		if err := m.l.Create(ctx, name, m.image, token, nil); err != nil {
			return "", err
		}
		m.tokens[name] = token
		return token, nil
	default:
		return "", fmt.Errorf("failed to look up sandbox %s: %w", name, err)
	}
}

// Delete removes a session's sandbox and its workspace.
func (m *Sandboxes) Delete(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("session ID is required")
	}
	return m.l.Delete(ctx, SandboxName(sessionID))
}

// Phase reports the sandbox's lifecycle phase.
func (l *SDKLauncher) Phase(ctx context.Context, name string) (v1.SandboxPhase, error) {
	sb, err := l.c.Sandboxes().Get(ctx, Workspace, name)
	if err != nil {
		return "", fmt.Errorf("failed to get sandbox: %w", err)
	}
	return sb.Status.Phase, nil
}
