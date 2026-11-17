package openshell

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/stretchr/testify/require"
)

func ocsf(msg string) v1.LogLine { return v1.LogLine{Level: "OCSF", Target: "ocsf", Message: msg} }

func TestParseDecision(t *testing.T) {
	tests := []struct {
		name string
		line v1.LogLine
		want Decision
		ok   bool
	}{
		{
			name: "allowed net", line: ocsf("NET:OPEN [INFO] ALLOWED /usr/lib/git-core/git-remote-http(0) -> github.com:443 [policy:durable-git engine:opa]"),
			want: Decision{Allowed: true, Kind: "NET", Binary: "/usr/lib/git-core/git-remote-http", Host: "github.com:443", Policy: "durable-git"}, ok: true,
		},
		{
			name: "denied net", line: ocsf("NET:OPEN [MED] DENIED /usr/local/bin/mise(0) -> api.github.com:443 [reason:transparent_tcp_policy_denied]"),
			want: Decision{Kind: "NET", Binary: "/usr/local/bin/mise", Host: "api.github.com:443", Reason: "transparent_tcp_policy_denied"}, ok: true,
		},
		{
			name: "allowed get", line: ocsf("HTTP:GET [INFO] ALLOWED GET https://github.com/o/r.git/info/refs [policy:durable-git engine:l7]"),
			want: Decision{Allowed: true, Kind: "HTTP", Method: "GET", Host: "github.com", Path: "/o/r.git/info/refs", Policy: "durable-git"}, ok: true,
		},
		{
			name: "allowed post", line: ocsf("HTTP:POST [INFO] ALLOWED POST https://github.com/o/r.git/git-receive-pack [policy:durable-git engine:l7]"),
			want: Decision{Allowed: true, Kind: "HTTP", Method: "POST", Host: "github.com", Path: "/o/r.git/git-receive-pack", Policy: "durable-git"}, ok: true,
		},
		{
			name: "denied get with reason", line: ocsf("HTTP:GET [MED] DENIED GET https://github.com/golang/go/info/refs [policy:durable-git engine:l7] [reason:L7_REQUEST deny GET github.com:443/golang/go/info/refs reason=GET /golang/go/info/refs not permitted by policy]"),
			want: Decision{Kind: "HTTP", Method: "GET", Host: "github.com", Path: "/golang/go/info/refs", Policy: "durable-git",
				Reason: "L7_REQUEST deny GET github.com:443/golang/go/info/refs reason=GET /golang/go/info/refs not permitted by policy"}, ok: true,
		},
		{
			name: "query is dropped", line: ocsf("HTTP:GET [INFO] ALLOWED GET https://release-assets.githubusercontent.com/a/b?sig=secret&se=1 [policy:mise engine:l7]"),
			want: Decision{Allowed: true, Kind: "HTTP", Method: "GET", Host: "release-assets.githubusercontent.com", Path: "/a/b", Policy: "mise"}, ok: true,
		},
		{name: "config line", line: ocsf("CONFIG:LOADED [INFO] Acknowledged initial policy revision as loaded [version:2] [hash:abc]")},
		{name: "not ocsf", line: v1.LogLine{Target: "other", Message: "NET:OPEN [INFO] ALLOWED /bin/x(0) -> a.com:443"}},
		{name: "empty message", line: ocsf("")},
		{name: "no verdict", line: ocsf("NET:OPEN [INFO] LISTENING /bin/x(0) -> a.com:443")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseDecision(tc.line)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFormatDecisionsGroupsAndSorts(t *testing.T) {
	ds := []Decision{
		{Allowed: true, Kind: "HTTP", Method: "GET", Host: "b.com", Path: "/v1.2.3/x", Policy: "p"},
		{Allowed: true, Kind: "HTTP", Method: "GET", Host: "b.com", Path: "/v2.0.0/x", Policy: "p"},
		{Kind: "NET", Binary: "/bin/curl", Host: "a.com:443"},
	}
	var buf bytes.Buffer
	require.NoError(t, FormatDecisions(&buf, ds))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 3)
	require.Equal(t, []string{"denied", "NET", "/bin/curl", "-", "a.com:443", "-", "-", "1"}, strings.Fields(lines[1]))
	require.Equal(t, []string{"allowed", "HTTP", "-", "GET", "b.com", "/*/x", "p", "2"}, strings.Fields(lines[2]))
}

func TestDecisionPatternGroupsVersionsAndHashes(t *testing.T) {
	tests := map[string]string{
		"/jqlang/jq/releases/download/jq-1.8.2/jq-linux-arm64":                                                       "/jqlang/jq/releases/download/*/jq-linux-arm64",
		"/api/github/repos/o/r/attestations/sha256:8b85c817833814ddca00a144c33705546355afccf0cf39b188f3cdb48b852309": "/api/github/repos/o/r/attestations/*",
		"/github-production-release-asset/123/bc90b8cc-1830-497a-99b0-4cd67ac89460":                                  "/github-production-release-asset/*/*",
		"/go/go1.27.4.linux-arm64.tar.gz":                                                                            "/go/*",
		"/data/jq.toml":                                                                                              "/data/jq.toml",
		"/v2/left-pad":                                                                                               "/*/left-pad",
		"":                                                                                                           "",
	}
	for in, want := range tests {
		require.Equal(t, want, Decision{Path: in}.Pattern(), in)
	}
}
