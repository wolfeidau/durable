package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// goWorkflow builds, tests and lints a module through the go-modules grant, then shows that other hosts are still denied.
const goWorkflow = `cd /sandbox && mkdir app && cd app && go mod init example.com/app` +
	` && printf 'package main\n\nimport (\n\t"fmt"\n\n\t"rsc.io/quote"\n)\n\nfunc main() { fmt.Println(quote.Go()) }\n' > main.go` +
	` && go mod tidy && go run . && golangci-lint run ./... && echo GO-OK;` +
	` curl -s -o /dev/null --max-time 10 https://example.com; echo curl-rc=$?; ls -A /sandbox`

func TestGatewayGoEnvironmentRunsModulesAndKeepsOtherHostsDenied(t *testing.T) {
	_, home, l := gatewayHome(t)
	writeEnvironments(t, home, `{"go": {"image": "durable-worker-go:dev", "grants": ["go-modules"]}}`)
	// The first lint run builds its cache, so the default 120s timeout is too short.
	input, err := json.Marshal(map[string]any{"command": goWorkflow, "timeout_seconds": 600})
	require.NoError(t, err)
	api := sequenceAPI(t,
		toolReply("toolu_go1", "bash", string(input)),
		textReply("done"),
	)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy")
	c := newClient(r)

	got, err := c.createEmptyIn(t, "go", "req-create-0123456789")
	require.NoError(t, err)
	id := got.GetSession().GetId()
	require.Equal(t, "go", got.GetSession().GetEnvironment())
	sessionPin(t, home, id, l)

	stream := openWatch(t, r, id, false)
	submitAs(t, c, id, "build it")
	approveSeq(t, c, id, 1)
	items := lastRun(t, collectRuns(t, stream, 1))

	out, ok := toolResult(items, "toolu_go1")
	require.True(t, ok)
	require.Contains(t, out, "GO-OK", out)
	require.Contains(t, out, "Don't communicate by sharing memory")
	require.Contains(t, out, "curl-rc=")
	require.NotContains(t, out, "curl-rc=0", "only the go binary may reach the module proxy")
	// HOME is the workspace, so a cache there would end up in commits.
	require.Regexp(t, `(?m)curl-rc=\d+\napp\s*$`, out)
}
