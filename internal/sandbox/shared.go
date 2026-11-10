package sandbox

import (
	"context"
	"sync"
)

// Shared wraps b so a sandbox stops only when its last user releases it, and ensure and stop never overlap for one
// name. The server closes an idle session after releasing its lease, so a reopen of the same session can ensure the
// sandbox before the old close stops it; without this the stop would land under the new session.
func Shared(b Backend) Backend {
	return &shared{Backend: b, names: map[string]*sharedName{}}
}

type shared struct {
	Backend

	mu    sync.Mutex
	names map[string]*sharedName
}

type sharedName struct {
	mu    sync.Mutex // held across the backend call, so ensure and stop for one name run one at a time
	users int
}

func (s *shared) entry(name string) *sharedName {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.names[name]
	if !ok {
		n = &sharedName{}
		s.names[name] = n
	}
	return n
}

func (s *shared) Ensure(ctx context.Context, spec Spec) error {
	n := s.entry(spec.Name)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := s.Backend.Ensure(ctx, spec); err != nil {
		return err
	}
	n.users++
	return nil
}

func (s *shared) Stop(ctx context.Context, name string) error {
	n := s.entry(name)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.users > 0 {
		n.users--
	}
	if n.users > 0 {
		return nil
	}
	return s.Backend.Stop(ctx, name)
}
