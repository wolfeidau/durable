package openshell

import (
	"context"
	_ "embed"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/commands"
	"github.com/wolfeidau/durable/internal/sandbox"
)

//go:embed testdata/mise/mise.toml
var miseFixtureConfig []byte

//go:embed testdata/mise/mise.lock
var miseFixtureLock []byte

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// miseBox is a sandbox with grants, whose commands report their exit code.
type miseBox struct {
	l    *Launcher
	name string
	run  func(command string) string
}

// miseSession starts a sandbox with the grants and returns a runner that appends the exit code to each command.
func miseSession(t *testing.T, grants []string) miseBox {
	t.Helper()
	l := gatewayLauncher(t)
	name, token := "dt-"+randHex(t, 6), randHex(t, 16)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		require.NoError(t, l.Delete(ctx, name))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	require.NoError(t, l.Create(ctx, sandbox.Spec{Name: name, Image: testImage(), Token: token, Grants: commands.BuildGrants(grants, nil)}))
	bash := bashTool(t, dialWorker(t, l, name, token))
	n := 0
	return miseBox{l: l, name: name, run: func(command string) string {
		n++
		return runBash(t.Context(), t, bash, "r"+string(rune('a'+n)), command+" 2>&1; echo rc=$?")
	}}
}

func TestGatewayMiseGrantInstallsFromTheLockfileOnly(t *testing.T) {
	box := miseSession(t, []string{"mise"})
	run := box.run

	out := run("echo " + b64(miseFixtureConfig) + " | base64 -d > mise.toml && echo " + b64(miseFixtureLock) + " | base64 -d > mise.lock")
	require.Contains(t, out, "rc=0", out)

	out = run("mise --version")
	require.NotContains(t, out, "releases.tsv", "the update check is allowed, so it doesn't retry")

	since := time.Now()
	out = run("mise install")
	require.Contains(t, out, "rc=0", out)
	require.NotContains(t, out, "policy_denied", out)
	requireDecisions(t, box.l, box.name, since, want{Allowed: true, Kind: "HTTP", Method: "GET", Host: "github.com", Path: "/*/*/releases/download/**", Policy: "mise"})

	out = run("mise exec -- jq --version")
	require.Contains(t, out, "rc=0", out)

	// The grant is bound to the mise binary, so curl to a release URL is refused.
	since = time.Now()
	out = run("curl -sS -m 20 -o /dev/null https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-arm64")
	require.NotContains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Kind: "NET", Binary: "**/curl", Host: "github.com:443"})

	// mise looks versions up through mise-versions and the GitHub API, and the grant allows neither.
	since = time.Now()
	out = run("mise ls-remote jq")
	require.NotContains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Kind: "NET", Binary: "/usr/local/bin/mise", Host: "mise-versions.jdx.dev:443"})
	since = time.Now()
	out = run("mise exec -- curl -sS -m 20 https://api.github.com/zen")
	require.NotContains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Kind: "NET", Binary: "**/curl", Host: "api.github.com:443"})
	requireNoDecision(t, box.l, box.name, time.Time{}, want{Allowed: true, Binary: "**/mise", Policy: "durable-git"})
}

// Each test works in a project directory, because mise use in HOME writes the global config, which has no lockfile.
const inMiseProject = "mkdir -p /sandbox/p && cd /sandbox/p && "

func TestGatewayMiseResolveAddsAndLocksATool(t *testing.T) {
	box := miseSession(t, []string{"mise", "mise-resolve"})
	run := box.run

	since := time.Now()
	out := run(inMiseProject + "MISE_LOCKED=0 mise use jq@1")
	require.Contains(t, out, "rc=0", out)
	require.NotContains(t, out, "policy_denied", out)
	requireDecisions(t, box.l, box.name, since,
		want{Allowed: true, Kind: "HTTP", Method: "GET", Host: "mise-versions.jdx.dev", Path: "/data/*", Policy: "mise-resolve"},
		want{Allowed: true, Kind: "HTTP", Method: "GET", Host: "github.com", Path: "/*/*/releases/download/**", Policy: "mise"},
	)

	out = run(inMiseProject + "MISE_LOCKED=0 mise lock --platform linux-arm64,linux-x64 && grep -c jq mise.lock")
	require.Contains(t, out, "rc=0", out)

	// With the lockfile written, the default locked mode runs the tool.
	out = run(inMiseProject + "mise exec -- jq --version")
	require.Contains(t, out, "rc=0", out)

	out = run(inMiseProject + "MISE_LOCKED=0 mise outdated")
	require.Contains(t, out, "rc=0", out)

	// The grant is bound to the mise binary, so curl is refused.
	since = time.Now()
	out = run("curl -sS -m 20 https://mise-versions.jdx.dev/data/jq.toml")
	require.NotContains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Kind: "NET", Binary: "**/curl", Host: "mise-versions.jdx.dev:443"})
}

// kubetail has no mise-versions data file, so mise falls back to the releases list through the versions host.
func TestGatewayMiseResolveListsReleases(t *testing.T) {
	box := miseSession(t, []string{"mise", "mise-resolve"})

	since := time.Now()
	out := box.run(inMiseProject + "set -o pipefail; MISE_LOCKED=0 mise ls-remote aqua:johanhaleby/kubetail | tail -2")
	require.Contains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Allowed: true, Kind: "HTTP", Method: "GET", Host: "mise-versions.jdx.dev", Path: "/api/github/repos/*/*/releases", Policy: "mise-resolve"})
}

func TestGatewayNpmRegistryForMiseBun(t *testing.T) {
	box := miseSession(t, []string{"mise", "mise-resolve", "npm-registry"})
	run := box.run

	out := run(inMiseProject + "MISE_LOCKED=0 mise use bun@1 && MISE_LOCKED=0 mise lock --platform linux-arm64,linux-x64")
	require.Contains(t, out, "rc=0", out)

	since := time.Now()
	out = run(inMiseProject + `echo '{"name":"x","dependencies":{"left-pad":"1.3.0"}}' > package.json && mise exec -- bun install`)
	require.Contains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Allowed: true, Kind: "HTTP", Method: "GET", Host: "registry.npmjs.org", Policy: "npm-registry"})

	since = time.Now()
	out = run("curl -sS -m 20 https://registry.npmjs.org/left-pad")
	require.NotContains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Kind: "NET", Binary: "**/curl", Host: "registry.npmjs.org:443"})
}

func TestGatewayGoModulesForMiseGo(t *testing.T) {
	box := miseSession(t, []string{"mise", "mise-resolve", "go-modules"})
	run := box.run

	since := time.Now()
	out := run(inMiseProject + "MISE_LOCKED=0 mise use go@1.27 && MISE_LOCKED=0 mise lock --platform linux-arm64,linux-x64")
	require.Contains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Allowed: true, Kind: "HTTP", Method: "GET", Host: "dl.google.com", Path: "/go/**", Policy: "mise"})

	since = time.Now()
	out = run(inMiseProject + "mise exec -- go mod init example.com/x && mise exec -- go get rsc.io/quote@v1.5.2")
	require.Contains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Allowed: true, Kind: "HTTP", Method: "GET", Host: "proxy.golang.org", Policy: "go-modules"})

	// Nothing but the granted hosts is reachable, even from a tool under mise exec.
	since = time.Now()
	out = run(inMiseProject + "mise exec -- curl -sS -m 20 https://example.com")
	require.NotContains(t, out, "rc=0", out)
	requireDecisions(t, box.l, box.name, since, want{Kind: "NET", Host: "example.com:443"})
}
