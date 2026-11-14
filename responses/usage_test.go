package responses

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
	"github.com/wolfeidau/durable/internal/fakeapi"
)

func TestUsageMapping(t *testing.T) {
	item := fakeapi.ResponsesMessageItem("msg_1", "hi")
	cases := []struct {
		name  string
		usage string
		want  *durable.Usage
	}{
		{"cached split out", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":50}`,
			&durable.Usage{InputTokens: 60, CacheReadTokens: 40, OutputTokens: 50}},
		{"reasoning and cache write", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":20},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":12}}`,
			&durable.Usage{InputTokens: 50, CacheReadTokens: 30, CacheWriteTokens: 20, OutputTokens: 50, ReasoningTokens: 12}},
		{"provider cost", `{"input_tokens":10,"output_tokens":5,"cost":0.0004}`,
			&durable.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.0004, CostSource: durable.CostProvider}},
		{"cached larger than input", `{"input_tokens":5,"input_tokens_details":{"cached_tokens":9}}`,
			&durable.Usage{CacheReadTokens: 9}},
		{"null", `null`, nil},
		{"malformed", `"lots"`, nil},
		{"wrong field types", `{"input_tokens":"many"}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, err := generate(t, newServer(t, ok(fakeapi.ResponsesCompletedUsage("["+item+"]", c.usage))), Config{})
			require.NoError(t, err)
			require.Equal(t, "hi", msg.Text)
			require.Equal(t, c.want, msg.Usage)
		})
	}

	msg, err := generate(t, newServer(t, ok(completed(item))), Config{})
	require.NoError(t, err)
	require.Nil(t, msg.Usage)
}

func TestUsageNeverReachesTheRequest(t *testing.T) {
	var bodies []string
	for _, u := range []*durable.Usage{nil, {InputTokens: 7, CostUSD: 1.5, CostSource: durable.CostProvider, Model: "m", DurationMs: 3}} {
		f := newServer(t, stubReply())
		history := []durable.Message{
			{Role: durable.RoleUser, Text: "hi"},
			{Role: durable.RoleAssistant, Text: "ok", ContextTokens: 9, Usage: u},
			{Role: durable.RoleUser, Text: "again"},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, err := f.model(Config{}).Generate(ctx, history, nil)
		cancel()
		require.NoError(t, err)
		bodies = append(bodies, f.bodies[0])
	}
	require.Equal(t, bodies[0], bodies[1])
}
