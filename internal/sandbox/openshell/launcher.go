// Package openshell runs the durable worker in an OpenShell sandbox through the Go SDK. Only the server links it.
package openshell

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
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
}

// Launcher manages worker sandboxes on one gateway.
type Launcher struct {
	c      v1.ClientInterface
	ws     string
	tokens map[string]func(ctx context.Context) (string, error)

	// mu guards synced and profiled, and serialises provider updates.
	mu sync.Mutex
	// synced holds the hash of the token last set per provider, never the token.
	synced   map[string][sha256.Size]byte
	profiled map[string]bool
}

// New connects to the gateway.
func New(cfg Config) (*Launcher, error) {
	gw, err := gateway.LoadConfig(cfg.Gateway)
	if err != nil {
		return nil, fmt.Errorf("failed to load gateway config: %w", err)
	}
	// The SDK has no mTLS auth mode, so the client certificate goes through the TLS config (see the OpenShell gaps in docs/remote-execution.md).
	c, err := gateway.NewClient(gw.Name,
		gateway.WithAuth(v1.NoAuth()),
		gateway.WithTLS(&types.TLSConfig{
			CertFile: filepath.Join(gw.Dir, "mtls", "tls.crt"),
			KeyFile:  filepath.Join(gw.Dir, "mtls", "tls.key"),
			CAFile:   filepath.Join(gw.Dir, "mtls", "ca.crt"),
		}))
	if err != nil {
		return nil, fmt.Errorf("failed to create gateway client: %w", err)
	}
	return newWithClient(c, cfg.Workspace, cfg.Tokens), nil
}

func newWithClient(c v1.ClientInterface, workspace string, tokens map[string]func(ctx context.Context) (string, error)) *Launcher {
	if workspace == "" {
		workspace = defaultWorkspace
	}
	return &Launcher{c: c, ws: workspace, tokens: tokens, synced: map[string][sha256.Size]byte{}, profiled: map[string]bool{}}
}

// Close releases the gateway connection.
func (l *Launcher) Close() error { return l.c.Close() }

// Create creates the sandbox and waits until it is ready. A sandbox that never gets ready is deleted, so a failed
// create leaves nothing behind whose token the caller may not have kept.
func (l *Launcher) Create(ctx context.Context, s sandbox.Spec) error {
	if s.Name == "" || s.Image == "" || s.Token == "" {
		return errors.New("sandbox name, image and worker token are required")
	}
	if err := l.syncProviders(ctx, s.Git); err != nil {
		return err
	}
	env := map[string]string{worker.TokenEnv: s.Token}
	maps.Copy(env, gitEnv(s.Git))
	spec := &v1.SandboxSpec{
		Environment: env,
		Providers:   gitProviders(s.Git),
		Template:    &v1.SandboxTemplate{Image: s.Image},
		Command:     []string{WorkerBinary, "--addr", "127.0.0.1:" + strconv.Itoa(WorkerPort), "--root", Root},
		// A worker that exits leaves the sandbox in Error. On OpenShell v0.1.7 OnFailure crash-loops in Starting,
		// where the sandbox can be neither stopped nor deleted.
		RestartPolicy: v1.SandboxRestartNever,
		Policy:        sandboxPolicy(s.Egress, s.Git),
	}
	if _, err := l.c.Sandboxes().Create(ctx, l.ws, s.Name, spec); err != nil {
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

// sandboxPolicy lets the worker binary, which runs the MCP bridge, reach the egress endpoints, and the repository
// clients reach only what access allows. Without a policy the sandbox has no egress at all. Giving one drops the
// gateway's default Landlock setting but keeps its filesystem and process defaults, so only Landlock is restated.
func sandboxPolicy(egress []sandbox.Endpoint, access []sandbox.GitAccess) *types.SandboxPolicy {
	if len(egress) == 0 && len(access) == 0 {
		return nil
	}
	rules := map[string]types.NetworkPolicyRule{}
	if len(egress) > 0 {
		endpoints := make([]types.PolicyNetworkEndpoint, 0, len(egress))
		for _, e := range egress {
			endpoints = append(endpoints, types.PolicyNetworkEndpoint{Host: e.Host, Port: e.Port})
		}
		rules[egressRule] = types.NetworkPolicyRule{Name: egressRule, Endpoints: endpoints, Binaries: []types.PolicyNetworkBinary{{Path: WorkerBinary}}}
	}
	if len(access) > 0 {
		rules[gitRule] = gitPolicyRule(access)
	}
	return &types.SandboxPolicy{
		Landlock:        &types.LandlockPolicy{Compatibility: "best_effort"},
		NetworkPolicies: rules,
	}
}

// gitPolicyRule merges accesses to the same provider and endpoint into one endpoint, because the gateway matches an
// endpoint once. The binding is what gives the sandbox the provider's credential placeholder.
func gitPolicyRule(access []sandbox.GitAccess) types.NetworkPolicyRule {
	type key struct {
		provider, host string
		port           uint32
	}
	var (
		order     []key
		endpoints = map[key]*types.PolicyNetworkEndpoint{}
		seen      = map[key]map[sandbox.HTTPRule]bool{}
		binaries  []types.PolicyNetworkBinary
	)
	for _, g := range access {
		k := key{g.Provider, g.Endpoint.Host, g.Endpoint.Port}
		ep := endpoints[k]
		if ep == nil {
			ep = &types.PolicyNetworkEndpoint{
				Host: g.Endpoint.Host, Port: g.Endpoint.Port, Protocol: "rest", Enforcement: types.NetworkEnforcementModeEnforce,
				CredentialBinding: &types.NetworkCredentialBinding{Provider: g.Provider},
			}
			endpoints[k] = ep
			seen[k] = map[sandbox.HTTPRule]bool{}
			order = append(order, k)
		}
		for _, r := range g.Rules {
			if !seen[k][r] {
				seen[k][r] = true
				ep.Rules = append(ep.Rules, types.L7Rule{Allow: &types.L7Allow{Method: r.Method, Path: r.Path}})
			}
		}
		for _, b := range g.Binaries {
			if !slices.Contains(binaries, types.PolicyNetworkBinary{Path: b}) {
				binaries = append(binaries, types.PolicyNetworkBinary{Path: b})
			}
		}
	}
	rule := types.NetworkPolicyRule{Name: gitRule, Binaries: binaries}
	for _, k := range order {
		rule.Endpoints = append(rule.Endpoints, *endpoints[k])
	}
	return rule
}

// gitProviders returns the providers the sandbox needs, sorted and unique.
func gitProviders(access []sandbox.GitAccess) []string {
	var names []string
	for _, g := range access {
		names = append(names, g.Provider)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// gitEnv numbers the git client's config entries in key order so the environment is the same on every create.
func gitEnv(access []sandbox.GitAccess) map[string]string {
	conf := map[string]string{}
	for _, g := range access {
		maps.Copy(conf, g.GitConfig)
	}
	if len(conf) == 0 {
		return nil
	}
	env := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(len(conf))}
	for i, k := range slices.Sorted(maps.Keys(conf)) {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = k
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = conf[k]
	}
	return env
}

// syncProviders makes sure each provider the sandbox uses has its profile and the host's current token. The gateway
// is only told when the token's hash differs from the last one this launcher set, so most opens make no update.
func (l *Launcher) syncProviders(ctx context.Context, access []sandbox.GitAccess) error {
	if len(access) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	done := map[string]bool{}
	for _, g := range access {
		if done[g.Provider] {
			continue
		}
		done[g.Provider] = true
		tokenFn := l.tokens[g.Kind]
		if tokenFn == nil {
			return fmt.Errorf("no %s token is configured for this server", g.Kind)
		}
		if err := l.ensureProfile(ctx, g); err != nil {
			return err
		}
		token, err := tokenFn(ctx)
		if err != nil {
			return fmt.Errorf("failed to get %s token: %w", g.Kind, err)
		}
		sum := sha256.Sum256([]byte(token))
		if last, ok := l.synced[g.Provider]; ok && last == sum {
			continue
		}
		_, err = l.c.Providers().Ensure(ctx, l.ws, &types.Provider{
			Name: g.Provider,
			Type: g.ProfileID,
			Spec: types.ProviderSpec{ProfileWorkspace: l.ws, Credentials: map[string]string{g.CredentialEnv: token}},
		})
		if err != nil {
			return fmt.Errorf("failed to set %s provider: %w", g.Kind, err)
		}
		l.synced[g.Provider] = sum
	}
	return nil
}

// ensureProfile imports the credential-only profile for g once. The request rules live in each sandbox's policy, so
// the profile has no endpoints.
func (l *Launcher) ensureProfile(ctx context.Context, g sandbox.GitAccess) error {
	if l.profiled[g.ProfileID] {
		return nil
	}
	_, err := l.c.Providers().Profiles().Get(ctx, l.ws, g.ProfileID)
	if err == nil {
		l.profiled[g.ProfileID] = true
		return nil
	}
	if !v1.IsNotFound(err) {
		return fmt.Errorf("failed to get %s profile: %w", g.Kind, err)
	}
	_, err = l.c.Providers().Profiles().Import(ctx, l.ws, []types.ProfileImportItem{{
		Source: "durable",
		Profile: types.ProviderProfile{
			ID:          g.ProfileID,
			DisplayName: "durable " + g.Kind + " token",
			Category:    types.ProfileCategorySourceControl,
			Credentials: []types.ProfileCredential{{
				Name: g.CredentialEnv, EnvVars: []string{g.CredentialEnv}, Required: true, Secret: true,
				AuthStyle: "bearer", HeaderName: "authorization",
			}},
		},
	}})
	if err != nil {
		return fmt.Errorf("failed to import %s profile: %w", g.Kind, err)
	}
	l.profiled[g.ProfileID] = true
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
