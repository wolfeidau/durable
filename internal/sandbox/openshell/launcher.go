// Package openshell runs the durable worker in an OpenShell sandbox through the Go SDK. Only the server links it.
package openshell

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/gateway"
	"github.com/ai-openshell/sdk-go/openshell/v1/types"

	"github.com/wolfeidau/durable/internal/sandbox"
	"github.com/wolfeidau/durable/internal/worker"
)

const (
	// WorkerPort is the port the worker listens on inside the sandbox.
	WorkerPort = 8080
	// WorkerBinary is where the worker image installs the worker.
	WorkerBinary = sandbox.WorkerBinary
	// Root is the workspace the worker's tools are confined to.
	Root = "/sandbox"

	egressRule       = "durable-mcp"
	gitRule          = "durable-git"
	grantRulePrefix  = "durable-grant-"
	defaultWorkspace = "default"
	deleteTimeout    = 2 * time.Minute
	stopPollInterval = 500 * time.Millisecond
)

// ErrNotFound reports that the sandbox does not exist.
var ErrNotFound = errors.New("sandbox not found")

// Config selects the gateway and workspace.
type Config struct {
	// Gateway names the gateway; empty means the active one.
	Gateway string
	// Workspace is the OpenShell workspace sandboxes live in; empty means "default".
	Workspace string
	// Tokens returns the current token for a git host kind. It is called whenever a sandbox with repository access is
	// created or started, so a rotated token takes effect without a restart.
	Tokens map[string]func(ctx context.Context) (string, error)
	// Apps returns the current GitHub App credentials for a git host kind.
	Apps map[string]func(ctx context.Context) (AppCredentials, error)
	// AppTokenURL overrides the GitHub App installation token URL template, for tests.
	AppTokenURL string
}

// Launcher manages worker sandboxes on one gateway.
type Launcher struct {
	c           v1.ClientInterface
	appRPC      appRefreshClient
	ws          string
	tokens      map[string]func(ctx context.Context) (string, error)
	apps        map[string]func(ctx context.Context) (AppCredentials, error)
	appTokenURL string

	// mu guards synced and profiled, and serialises provider updates.
	mu sync.Mutex
	// synced holds the hash of the credential material last set per provider, never the secret.
	synced   map[string][sha256.Size]byte
	profiled map[string]bool
}

// New connects to the gateway.
func New(cfg Config) (*Launcher, error) {
	gw, err := gateway.LoadConfig(cfg.Gateway)
	if err != nil {
		return nil, fmt.Errorf("failed to load gateway config: %w", err)
	}
	tlsCfg := &types.TLSConfig{
		CertFile: filepath.Join(gw.Dir, "mtls", "tls.crt"),
		KeyFile:  filepath.Join(gw.Dir, "mtls", "tls.key"),
		CAFile:   filepath.Join(gw.Dir, "mtls", "ca.crt"),
	}
	// The SDK has no mTLS auth mode, so the client certificate goes through the TLS config (gateway.NewClient rejects mTLS gateways).
	c, err := gateway.NewClient(gw.Name, gateway.WithAuth(v1.NoAuth()), gateway.WithTLS(tlsCfg))
	if err != nil {
		return nil, fmt.Errorf("failed to create gateway client: %w", err)
	}
	appRPC, err := dialAppRefreshClient(gw.Endpoint, tlsCfg)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	l := newWithClient(c, cfg.Workspace, cfg.Tokens)
	l.appRPC = appRPC
	l.apps = cfg.Apps
	l.appTokenURL = cfg.AppTokenURL
	return l, nil
}

func newWithClient(c v1.ClientInterface, workspace string, tokens map[string]func(ctx context.Context) (string, error)) *Launcher {
	if workspace == "" {
		workspace = defaultWorkspace
	}
	return &Launcher{c: c, ws: workspace, tokens: tokens, synced: map[string][sha256.Size]byte{}, profiled: map[string]bool{}}
}

// Close releases the gateway connection.
func (l *Launcher) Close() error {
	err := l.c.Close()
	if l.appRPC != nil {
		err = errors.Join(err, l.appRPC.Close())
	}
	return err
}

// Create creates the sandbox and waits until it is ready. A sandbox that never gets ready is deleted, so a failed
// create leaves nothing behind whose token the caller may not have kept.
func (l *Launcher) Create(ctx context.Context, s sandbox.Spec) error {
	if s.Name == "" || s.Image == "" || s.Token == "" {
		return errors.New("sandbox name, image and worker token are required")
	}
	if err := checkGrantProviders(s.Grants, s.Git); err != nil {
		return err
	}
	if err := l.syncProviders(ctx, s.Git); err != nil {
		return err
	}
	env := map[string]string{worker.TokenEnv: s.Token}
	maps.Copy(env, gitEnv(s.GitConfig, s.Git))
	spec := &v1.SandboxSpec{
		Environment: env,
		Providers:   gitProviders(s.Git),
		Template:    &v1.SandboxTemplate{Image: s.Image},
		Command:     []string{WorkerBinary, "--addr", "127.0.0.1:" + strconv.Itoa(WorkerPort), "--root", Root},
		// A worker that exits leaves the sandbox in Error. On OpenShell v0.1.7 OnFailure crash-loops in Starting,
		// where the sandbox can be neither stopped nor deleted.
		RestartPolicy: v1.SandboxRestartNever,
		Policy:        sandboxPolicy(s.Egress, s.Git, s.Grants),
	}
	if _, err := l.c.Sandboxes().Create(ctx, l.ws, s.Name, spec); err != nil {
		l.forget(s.Git)
		return fmt.Errorf("failed to create sandbox: %w", err)
	}
	if err := l.waitReady(ctx, s.Name); err != nil {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
		defer cancel()
		if derr := l.Delete(dctx, s.Name); derr != nil {
			return errors.Join(err, derr)
		}
		return err
	}
	return nil
}

// Ensure creates the sandbox when it is missing, else starts it.
func (l *Launcher) Ensure(ctx context.Context, s sandbox.Spec) error {
	if _, err := l.Phase(ctx, s.Name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return l.Create(ctx, s)
		}
		return err
	}
	// Start takes only a name, so the provider's token is refreshed here.
	if err := l.syncProviders(ctx, s.Git); err != nil {
		return err
	}
	return l.Start(ctx, s.Name)
}

// Start makes sure the sandbox is running and ready, so it is safe to call on every session open.
func (l *Launcher) Start(ctx context.Context, name string) error {
	sb, err := l.get(ctx, name)
	if err != nil {
		return err
	}
	phase := sb.Status.Phase
	switch phase {
	case types.SandboxReady:
		return nil
	case types.SandboxStopped:
		return l.start(ctx, name)
	case types.SandboxStopping:
		if _, err := l.c.Sandboxes().WaitStopped(ctx, l.ws, name); err != nil {
			return fmt.Errorf("failed to wait for sandbox to stop: %w", err)
		}
		return l.start(ctx, name)
	case types.SandboxProvisioning, types.SandboxStarting:
		return l.waitReady(ctx, name)
	case types.SandboxError:
		// A gateway restart leaves the container stopped but the sandbox in Error (NVIDIA/OpenShell#4910); stop then start recovers it.
		if !startFailed(sb) {
			return fmt.Errorf("sandbox %q is in phase %s", name, phase)
		}
		if err := l.stopFromError(ctx, name); err != nil {
			return err
		}
		return l.start(ctx, name)
	default:
		return fmt.Errorf("sandbox %q is in phase %s", name, phase)
	}
}

func (l *Launcher) start(ctx context.Context, name string) error {
	if _, err := l.c.Sandboxes().Start(ctx, l.ws, name); err != nil {
		return fmt.Errorf("failed to start sandbox: %w", err)
	}
	return l.waitReady(ctx, name)
}

func (l *Launcher) waitReady(ctx context.Context, name string) error {
	sb, err := l.c.Sandboxes().WaitReady(ctx, l.ws, name)
	if err != nil {
		return fmt.Errorf("failed to wait for sandbox: %w", err)
	}
	// WaitReady also returns a Completed sandbox, whose worker has exited.
	if sb.Status.Phase != types.SandboxReady {
		return fmt.Errorf("sandbox %q is %s, not Ready: its worker exited", name, sb.Status.Phase)
	}
	return nil
}

// Stop stops the sandbox and waits until it has stopped. A missing sandbox is not an error.
func (l *Launcher) Stop(ctx context.Context, name string) error {
	if _, err := l.c.Sandboxes().Stop(ctx, l.ws, name); err != nil {
		if v1.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to stop sandbox: %w", err)
	}
	if _, err := l.c.Sandboxes().WaitStopped(ctx, l.ws, name); err != nil {
		if v1.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to wait for sandbox to stop: %w", err)
	}
	return nil
}

// stopFromError stops a sandbox in Error and polls until it is Stopped. WaitStopped can't be used here, because it
// fails at once on Error, which the sandbox may still report just after the stop is requested.
func (l *Launcher) stopFromError(ctx context.Context, name string) error {
	if _, err := l.c.Sandboxes().Stop(ctx, l.ws, name); err != nil {
		return fmt.Errorf("failed to stop sandbox: %w", err)
	}
	t := time.NewTicker(stopPollInterval)
	defer t.Stop()
	for {
		phase, err := l.Phase(ctx, name)
		if err != nil {
			return err
		}
		if phase == types.SandboxStopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("failed to wait for sandbox to stop from %s: %w", phase, ctx.Err())
		case <-t.C:
		}
	}
}

// Delete removes the sandbox. A missing sandbox is not an error.
func (l *Launcher) Delete(ctx context.Context, name string) error {
	if _, err := l.c.Sandboxes().Delete(ctx, l.ws, name, v1.DeleteOptions{AllowMissing: true}); err != nil {
		return fmt.Errorf("failed to delete sandbox: %w", err)
	}
	return nil
}

// Phase returns the sandbox's phase, or ErrNotFound if it does not exist.
func (l *Launcher) Phase(ctx context.Context, name string) (types.SandboxPhase, error) {
	sb, err := l.get(ctx, name)
	if err != nil {
		return "", err
	}
	return sb.Status.Phase, nil
}

// Phases returns the neutral lifecycle phase of every sandbox in the launcher's workspace.
func (l *Launcher) Phases(ctx context.Context) (map[string]sandbox.Phase, error) {
	sbs, err := l.c.Sandboxes().ListAll(ctx, l.ws)
	if err != nil {
		return nil, fmt.Errorf("failed to list sandboxes: %w", err)
	}
	phases := make(map[string]sandbox.Phase, len(sbs))
	for _, sb := range sbs {
		phases[sb.Name] = mapPhase(sb.Status.Phase)
	}
	return phases, nil
}

func mapPhase(p types.SandboxPhase) sandbox.Phase {
	switch p {
	case types.SandboxReady:
		return sandbox.PhaseRunning
	case types.SandboxProvisioning, types.SandboxStarting:
		return sandbox.PhaseStarting
	case types.SandboxStopping, types.SandboxSuspending, types.SandboxDeleting:
		return sandbox.PhaseStopping
	case types.SandboxStopped, types.SandboxSuspended:
		return sandbox.PhaseStopped
	case types.SandboxError, types.SandboxCompleted:
		// Completed means the long-running worker exited unexpectedly and Start refuses it.
		return sandbox.PhaseError
	default:
		// Preserve Unknown and unrecognized phases so callers distinguish them from a missing sandbox.
		return sandbox.PhaseUnknown
	}
}

func (l *Launcher) get(ctx context.Context, name string) (*types.Sandbox, error) {
	sb, err := l.c.Sandboxes().Get(ctx, l.ws, name)
	if err != nil {
		if v1.IsNotFound(err) {
			return nil, fmt.Errorf("sandbox %q: %w", name, ErrNotFound)
		}
		return nil, fmt.Errorf("failed to get sandbox: %w", err)
	}
	return sb, nil
}

// startFailed reports whether the Ready condition says the sandbox failed to start, as opposed to its worker failing.
func startFailed(sb *types.Sandbox) bool {
	for _, c := range sb.Status.Conditions {
		if c.Type == "Ready" && c.Reason == "StartFailed" {
			return true
		}
	}
	return false
}

// IsUnavailable reports whether err is the gateway or worker being unreachable or slow, which a retry may fix.
// Everything else is configuration, such as a bad image.
func IsUnavailable(err error) bool {
	return v1.IsUnavailable(err) || v1.IsDeadlineExceeded(err) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errWorkerUnreachable)
}

// IsUnavailable implements sandbox.Backend.
func (*Launcher) IsUnavailable(err error) bool { return IsUnavailable(err) }

var _ sandbox.Backend = (*Launcher)(nil)
