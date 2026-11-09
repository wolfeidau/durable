// Command workspace is a throwaway spike: a sandbox clones and pushes a private repo through the tool worker,
// with the GitHub credential held by an OpenShell provider instead of the agent.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ai-openshell/sdk-go/openshell/v1/types"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/remote"
)

var probe string

// gitAuth sends the placeholder as the Basic password, which the proxy decodes and resolves; a Bearer header got a 401.
const gitAuth = `-c credential.helper='!f() { echo username=x-access-token; echo "password=$GITHUB_TOKEN"; }; f'`

const (
	providerName = "durable-gh-spike"
	profileID    = "durable-gh-spike"
	repo         = "wolfeidau/durable-sandbox-scratch"
	branch       = "spike/sandbox-push"
)

func main() {
	users := flag.Bool("users", false, "probe tool-user options instead of the clone and push workflow")
	flag.StringVar(&probe, "probe", "", "file of bash commands, one per line, to run after setup before stopping")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var err error
	if *users {
		err = runUsers(ctx)
	} else {
		err = run(ctx)
	}
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
}

func step(title string) { fmt.Printf("\n== %s\n", title) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// agent calls a sandbox's tools the way the harness would, and scrubs the real token from anything it prints.
type agent struct {
	tools map[string]durable.Tool
	token string
}

func newAgent(ctx context.Context, l *remote.SDKLauncher, name, workerToken, token string) (*agent, error) {
	c := l.Dial(name, workerToken, remote.WorkerPort)
	if err := c.WaitReady(ctx, 30*time.Second); err != nil {
		return nil, fmt.Errorf("worker not ready: %w", err)
	}
	ts, err := c.Tools(ctx)
	if err != nil {
		return nil, err
	}
	a := &agent{tools: map[string]durable.Tool{}, token: token}
	for _, t := range ts {
		a.tools[t.Spec().Name] = t
	}
	return a, nil
}

func (a *agent) call(ctx context.Context, name, input string) (string, error) {
	out, err := a.tools[name].Execute(ctx, durable.ToolCall{ID: "call-" + randHex(6), Name: name, Input: json.RawMessage(input)})
	return a.scrub(out), err
}

func (a *agent) scrub(s string) string {
	if a.token != "" {
		s = strings.ReplaceAll(s, a.token, "<REAL-TOKEN-LEAK>")
	}
	return s
}

func (a *agent) bash(ctx context.Context, cmd string) (string, error) {
	b, _ := json.Marshal(map[string]any{"command": cmd, "timeout_seconds": 120})
	return a.call(ctx, "bash", string(b))
}

func (a *agent) show(ctx context.Context, cmd string) (string, error) {
	out, err := a.bash(ctx, cmd)
	fmt.Printf("   $ %s\n%s   err=%v\n", cmd, indent(out), err)
	return out, err
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "      " + l
	}
	return strings.Join(lines, "\n") + "\n"
}

func ghToken(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func sameSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// profileYAML is the stock GitHub profile with the endpoint narrowed to one repository and the binaries to the git client.
const profileYAML = `id: durable-gh-spike
display_name: GitHub (durable spike)
description: GitHub token placed by the proxy for git
category: source_control
credentials:
  - name: api_token
    description: GitHub token
    env_vars: [GITHUB_TOKEN, GH_TOKEN]
    required: true
    auth_style: bearer
    header_name: authorization
discovery:
  credentials: [api_token]
endpoints:
  - host: github.com
    port: 443
    protocol: rest
    enforcement: enforce
    rules:
      - allow: { method: GET, path: "/` + repo + `.git/info/refs*" }
      - allow: { method: POST, path: "/` + repo + `.git/git-upload-pack" }
      - allow: { method: POST, path: "/` + repo + `.git/git-receive-pack" }
binaries: [/usr/bin/git, /usr/lib/git-core/git-remote-https]
`

func importProfile(ctx context.Context) error {
	f, err := os.CreateTemp("", "profile-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create profile file: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(profileYAML); err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to write profile: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close profile: %w", err)
	}
	return remote.ImportProfile(ctx, f.Name())
}

func policy() *types.SandboxPolicy {
	gitBins := []types.PolicyNetworkBinary{{Path: "/usr/bin/git"}, {Path: "/usr/lib/git-core/git-remote-https"}}
	allow := func(method, path string) types.L7Rule {
		return types.L7Rule{Allow: &types.L7Allow{Method: method, Path: path}}
	}
	return &types.SandboxPolicy{
		NetworkPolicies: map[string]types.NetworkPolicyRule{
			"github_repository_push": {
				Name: "github-repository-push",
				Endpoints: []types.PolicyNetworkEndpoint{{
					Host: "github.com", Port: 443, Protocol: "rest", Enforcement: types.NetworkEnforcementModeEnforce,
					Rules: []types.L7Rule{
						allow("GET", "/"+repo+".git/info/refs*"),
						allow("POST", "/"+repo+".git/git-upload-pack"),
						allow("POST", "/"+repo+".git/git-receive-pack"),
					},
				}},
				Binaries: gitBins,
			},
		},
	}
}

//nolint:gocognit // linear spike script, one block per step
func run(ctx context.Context) (err error) {
	token, err := ghToken(ctx)
	if err != nil {
		return err
	}
	l, err := remote.NewSDKLauncher(ctx)
	if err != nil {
		return err
	}
	defer l.Close()

	sbName := "dws-" + randHex(3)
	defer func() {
		// Runs on failure and interrupt too, so use a fresh context.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if derr := l.Delete(cctx, sbName); derr != nil {
			fmt.Fprintln(os.Stderr, "cleanup:", derr)
		}
		// Sandbox deletion is asynchronous, and the provider can't go while a sandbox still has it attached.
		for _, del := range []func() error{
			func() error { return l.DeleteProvider(cctx, providerName) },
			func() error { return l.DeleteProfile(cctx, profileID) },
		} {
			if derr := retry(cctx, del); derr != nil {
				fmt.Fprintln(os.Stderr, "cleanup:", derr)
			}
		}
	}()

	step("1. build image")
	if err := remote.BuildImage(ctx); err != nil {
		return err
	}

	step("2. profile and provider (SDK; the token goes only to the gateway)")
	if err := l.DeleteProvider(ctx, providerName); err != nil {
		return err
	}
	if err := l.DeleteProfile(ctx, profileID); err != nil {
		return err
	}
	if err := importProfile(ctx); err != nil {
		return err
	}
	if err := l.CreateProvider(ctx, providerName, profileID, map[string]string{"GITHUB_TOKEN": token}); err != nil {
		return err
	}
	fmt.Printf("   provider %s created (type %s)\n", providerName, profileID)

	step("3. create sandbox with provider and policy")
	workerToken := randHex(16)
	if err := l.CreateWith(ctx, sbName, remote.Image, workerToken, nil,
		remote.CreateOptions{Providers: []string{providerName}, Policy: policy()}); err != nil {
		if lines, lerr := l.Logs(ctx, sbName); lerr == nil {
			for _, ln := range lines[max(0, len(lines)-12):] {
				fmt.Println("   log:", truncate(ln, 300))
			}
		}
		return err
	}
	a, err := newAgent(ctx, l, sbName, workerToken, token)
	if err != nil {
		return err
	}
	fmt.Printf("   sandbox %s ready\n", sbName)
	if probe != "" {
		lines, rerr := os.ReadFile(probe) //nolint:gosec // spike flag, the operator's own file
		if rerr != nil {
			return fmt.Errorf("failed to read probe file: %w", rerr)
		}
		for c := range strings.SplitSeq(strings.TrimSpace(string(lines)), "\n") {
			_, _ = a.show(ctx, c)
		}
		dumpLogs(ctx, l, sbName)
		return nil
	}

	step("4. workspace through the tools only")
	for _, c := range []string{
		"git config --global user.name 'durable spike' && git config --global user.email 'spike@example.invalid'",
		"cd /sandbox && git " + gitAuth + " clone https://github.com/" + repo + ".git work 2>&1 | tail -5",
	} {
		if _, err := a.show(ctx, c); err != nil {
			dumpLogs(ctx, l, sbName)
			return err
		}
	}
	out, err := a.call(ctx, "read_file", `{"path":"work/README.md","offset":null,"limit":null}`)
	fmt.Printf("   read_file README.md err=%v\n%s", err, indent(out))
	if err != nil {
		dumpLogs(ctx, l, sbName)
		return err
	}
	first := strings.TrimSpace(strings.SplitN(stripLineNumbers(out), "\n", 2)[0])
	if first == "" {
		return errors.New("README.md is empty")
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	// edit_file needs a unique anchor, so the first line of the README is replaced by itself plus the new line.
	in, _ := json.Marshal(map[string]string{"path": "work/README.md", "old_string": first, "new_string": first + "\n\nspike push " + stamp})
	out, err = a.call(ctx, "edit_file", string(in))
	fmt.Printf("   edit_file err=%v %s\n", err, strings.TrimSpace(out))
	if err != nil {
		return err
	}
	for _, c := range []string{
		"cd /sandbox/work && git commit -am 'spike: sandbox push' 2>&1 | tail -3",
		"cd /sandbox/work && git " + gitAuth + " push origin HEAD:" + branch + " 2>&1 | tail -8",
	} {
		if _, err := a.show(ctx, c); err != nil {
			dumpLogs(ctx, l, sbName)
			return err
		}
	}
	sha, err := a.bash(ctx, "cd /sandbox/work && git rev-parse HEAD")
	if err != nil {
		return err
	}
	sha = strings.TrimSpace(sha)

	step("5. verify on the host")
	got, err := exec.CommandContext(ctx, "gh", "api", "repos/"+repo+"/branches/"+branch, "--jq", ".commit.sha").Output()
	if err != nil {
		return fmt.Errorf("gh api failed: %w", err)
	}
	fmt.Printf("   sandbox HEAD  %s\n   host branch   %s\n", sha, strings.TrimSpace(string(got)))
	if strings.TrimSpace(string(got)) != sha {
		return errors.New("pushed SHA does not match")
	}
	fmt.Println("   match")

	step("6. what the agent sees")
	if err := evidence(ctx, a, token); err != nil {
		return err
	}
	dumpLogs(ctx, l, sbName)
	return nil
}

// stripLineNumbers removes the "N<tab>" prefixes read_file adds.
func stripLineNumbers(s string) string {
	var out []string
	for ln := range strings.SplitSeq(s, "\n") {
		if _, rest, ok := strings.Cut(ln, "\t"); ok {
			ln = rest
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

func evidence(ctx context.Context, a *agent, token string) error {
	out, _ := a.bash(ctx, `env | grep -i -E 'gh|github|token'`)
	for ln := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		name, val, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		fmt.Printf("   env %-24s equals real token: %-5v placeholder-shaped: %v\n", name, sameSecret(val, token), strings.Contains(val, "openshell"))
	}
	if strings.Contains(out, "<REAL-TOKEN-LEAK>") {
		return errors.New("real token visible in the agent's environment")
	}
	fmt.Println("   real token appears nowhere in the agent's env output")
	_, _ = a.show(ctx, "curl -sS -m 5 https://example.com; echo exit=$?")
	_, _ = a.show(ctx, "curl -sS -m 5 https://api.github.com/user; echo exit=$?")
	_, _ = a.show(ctx, "cd /sandbox/work && git ls-remote https://github.com/wolfeidau/durable.git 2>&1 | head -3")
	return nil
}

func dumpLogs(ctx context.Context, l *remote.SDKLauncher, name string) {
	lines, err := l.Logs(context.WithoutCancel(ctx), name)
	if err != nil {
		fmt.Println("   logs:", err)
		return
	}
	fmt.Println("   -- sandbox log (policy decisions)")
	for _, ln := range lines {
		if strings.Contains(ln, "DENIED") || strings.Contains(ln, "ALLOWED") {
			fmt.Println("   log:", truncate(ln, 260))
		}
	}
}

func retry(ctx context.Context, fn func() error) error {
	var err error
	for range 30 {
		if err = fn(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
