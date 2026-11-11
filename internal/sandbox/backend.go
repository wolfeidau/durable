package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wolfeidau/durable"
)

const (
	// PinFile is the session's sandbox pin, next to its database.
	PinFile     = "sandbox.json"
	stopTimeout = 60 * time.Second
)

// Spec describes a sandbox to create.
type Spec struct {
	Name  string
	Image string
	// Token is the worker's bearer token, passed in the sandbox environment only.
	Token string
	// Egress is where the sandbox may connect, scoped to the worker binary.
	Egress []Endpoint
	// Git is the repository access the sandbox gets through its host's provider.
	Git []GitAccess
}

// GitAccess is a sandbox's access to one repository on a git host, as neutral data a backend turns into policy.
type GitAccess struct {
	Kind string `json:"kind"`
	// Provider and ProfileID name the backend's credential holder for the host, and CredentialEnv is the variable the sandbox sees.
	Provider      string   `json:"provider"`
	ProfileID     string   `json:"profile_id"`
	CredentialEnv string   `json:"credential_env"`
	Endpoint      Endpoint `json:"endpoint"`
	// Rules are the only requests allowed to Endpoint.
	Rules    []HTTPRule `json:"rules"`
	Binaries []string   `json:"binaries"`
	// GitConfig is passed to the client through GIT_CONFIG_* variables.
	GitConfig map[string]string `json:"git_config,omitempty"`
}

// HTTPRule allows one method and path pattern.
type HTTPRule struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Endpoint is a host and port a sandbox may reach.
type Endpoint struct {
	Host string `json:"host"`
	Port uint32 `json:"port"`
}

// Backend runs sandboxes.
type Backend interface {
	// Ensure creates the sandbox when it is missing, else starts it, and returns once it is ready.
	Ensure(ctx context.Context, s Spec) error
	Stop(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
	Connect(ctx context.Context, name, token string) (*Client, io.Closer, error)
	// IsUnavailable reports whether err is the backend being unreachable or slow, which a retry may fix.
	IsUnavailable(err error) bool
}

// Error is a failure to open a session's sandbox.
type Error struct {
	// Unavailable is set when a retry may fix it; otherwise it is configuration.
	Unavailable bool
	Err         error
}

func (e *Error) Error() string { return "sandbox: " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Pin is a session's sandbox, written once before the sandbox is created so a failed create leaves its token on disk.
type Pin struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Token string `json:"token"`
	// Egress is fixed at the first pin, so a recreated sandbox gets the same rule. Pins from before it have none.
	Egress []Endpoint `json:"egress,omitempty"`
	// Git is fixed at the first pin, like Egress.
	Git []GitAccess `json:"git,omitempty"`
}

// Spec returns the pin as a sandbox spec.
func (p Pin) Spec() Spec { return Spec(p) }

// ReadPin reads the pin in dir; ok is false when the session has none.
func ReadPin(dir string) (p Pin, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, PinFile)) //nolint:gosec // fixed name inside the session directory
	if errors.Is(err, fs.ErrNotExist) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, fmt.Errorf("failed to read sandbox pin: %w", err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return Pin{}, false, fmt.Errorf("failed to parse sandbox pin: %w", err)
	}
	if p.Name == "" || p.Image == "" || p.Token == "" {
		return Pin{}, false, errors.New("sandbox pin is incomplete")
	}
	return p, true, nil
}

// WritePin pins image and egress for the session in dir with a fresh random name and token. The first writer wins, and
// every caller gets the winner's pin back.
func WritePin(dir, image string, egress ...Endpoint) (Pin, error) {
	return WritePinGit(dir, image, egress, nil)
}

// WritePinGit is WritePin with repository access, which is fixed at the first pin too.
func WritePinGit(dir, image string, egress []Endpoint, repos []GitAccess) (Pin, error) {
	if p, ok, err := ReadPin(dir); ok || err != nil {
		return p, err
	}
	name, err := randHex(8)
	if err != nil {
		return Pin{}, err
	}
	token, err := randHex(32)
	if err != nil {
		return Pin{}, err
	}
	// The name is random, not derived from the session, so homes never collide; "ds-" plus 16 hex fits OpenShell's name limit.
	data, err := json.Marshal(Pin{Name: "ds-" + name, Image: image, Token: token, Egress: egress, Git: repos})
	if err != nil {
		return Pin{}, fmt.Errorf("failed to encode sandbox pin: %w", err)
	}
	if err := writeOnce(dir, filepath.Join(dir, PinFile), data); err != nil {
		return Pin{}, err
	}
	p, ok, err := ReadPin(dir)
	if err != nil {
		return Pin{}, err
	}
	if !ok {
		return Pin{}, errors.New("sandbox pin disappeared")
	}
	return p, nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// writeOnce durably creates path with content unless it exists. It copies the helper in internal/commands, which
// imports this package.
func writeOnce(dir, path string, content []byte) error {
	tmp, err := os.CreateTemp(dir, "pin-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create pin file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to set pin file mode: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write pin file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to sync pin file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close pin file: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("failed to pin %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Open readies the pinned sandbox and returns tools that run in it, and the client for its worker. The tools equal the server's catalog in names,
// order and specs, because Client.Tools enforces it. release closes the connection and stops the sandbox, or deletes it
// when discard is true; it is safe to call more than once and returns the error for the caller to log. A fresh sandbox
// is one this session's first open may have created, so a failed open deletes it instead of leaving a stale one for
// every retry to reuse.
func Open(ctx context.Context, b Backend, pin Pin, fresh bool) (toolset []durable.Tool, client *Client, release func(discard bool) error, err error) {
	if err := b.Ensure(ctx, pin.Spec()); err != nil {
		return nil, nil, nil, &Error{Unavailable: b.IsUnavailable(err), Err: err}
	}
	var (
		once   sync.Once
		closer io.Closer
	)
	release = func(discard bool) error {
		var rerr error
		once.Do(func() {
			if closer != nil {
				rerr = closer.Close()
			}
			// Detached so a cancelled request doesn't leave the sandbox running.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
			defer cancel()
			if discard {
				rerr = errors.Join(rerr, b.Delete(sctx, pin.Name))
				return
			}
			rerr = errors.Join(rerr, b.Stop(sctx, pin.Name))
		})
		return rerr
	}
	// A failed open releases the sandbox it started, and reports the open error first.
	fail := func(err error) error {
		oe := &Error{Unavailable: b.IsUnavailable(err), Err: err}
		if rerr := release(fresh); rerr != nil {
			return errors.Join(oe, rerr)
		}
		return oe
	}
	client, closer, err = b.Connect(ctx, pin.Name, pin.Token)
	if err != nil {
		return nil, nil, nil, fail(err)
	}
	toolset, err = client.Tools(ctx)
	if err != nil {
		return nil, nil, nil, fail(err)
	}
	return toolset, client, release, nil
}
