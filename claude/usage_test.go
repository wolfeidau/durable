package claude

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

func TestUsageMapping(t *testing.T) {
	blocks := fakeapi.TextBlock(0, "hi")
	cases := []struct {
		name         string
		start, delta string
		want         durable.Usage
	}{
		{"counts", `{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":1000,"output_tokens":1}`, `{"output_tokens":7}`,
			durable.Usage{InputTokens: 10, CacheReadTokens: 100, CacheWriteTokens: 1000, OutputTokens: 7}},
		{"provider cost", `{"input_tokens":10,"output_tokens":1}`, `{"output_tokens":7,"cost":0.0125}`,
			durable.Usage{InputTokens: 10, OutputTokens: 7, CostUSD: 0.0125, CostSource: durable.CostProvider}},
		{"no usage", `{}`, `{}`, durable.Usage{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := fakeapi.StreamUsage("claude-sonnet-5-5", "end_turn", "null", c.start, c.delta, blocks)
			msg, err := gen(t, newFakeAPI(t, reply{status: 200, body: body}).model(Config{}), []durable.Message{{Role: durable.RoleUser, Text: "hi"}}, nil)
			require.NoError(t, err)
			require.NotNil(t, msg.Usage)
			require.Equal(t, c.want, *msg.Usage)
		})
	}
}

func TestUsageNeverReachesTheRequest(t *testing.T) {
	history := func(u *durable.Usage) []durable.Message {
		return []durable.Message{
			{Role: durable.RoleUser, Text: "hi"},
			{Role: durable.RoleAssistant, Text: "ok", ContextTokens: 9, Usage: u},
			{Role: durable.RoleUser, Text: "again"},
		}
	}
	var bodies []string
	for _, u := range []*durable.Usage{nil, {InputTokens: 7, CostUSD: 1.5, CostSource: durable.CostProvider, Model: "m", DurationMs: 3}} {
		f := newFakeAPI(t, stream("end_turn", fakeapi.TextBlock(0, "ok")))
		_, err := gen(t, f.model(Config{}), history(u), nil)
		require.NoError(t, err)
		bodies = append(bodies, f.bodies[0])
	}
	require.Equal(t, bodies[0], bodies[1])
}
