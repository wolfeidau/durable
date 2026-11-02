package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

func newLive() (*liveWriter, *boundedWriter, *[]string) {
	dst := newBoundedWriter(maxOutputBytes)
	var got []string
	return &liveWriter{dst: dst, emit: func(s string) { got = append(got, s) }}, dst, &got
}

func TestLiveWriterHoldsBackASplitRune(t *testing.T) {
	w, dst, got := newLive()
	euro := []byte("€") // 3 bytes
	for _, p := range [][]byte{[]byte("a"), euro[:1], euro[1:2], append(append([]byte{}, euro[2:]...), 'b')} {
		_, err := w.Write(p)
		require.NoError(t, err)
	}
	require.Equal(t, "a€b", strings.Join(*got, ""))
	for _, s := range *got {
		require.Equal(t, s, strings.ToValidUTF8(s, ""))
	}
	require.Equal(t, "a€b", dst.String())
}

func TestLiveWriterReplacesInvalidBytesAndKeepsTheResult(t *testing.T) {
	w, dst, got := newLive()
	_, err := w.Write([]byte{'a', 0xff, 'b', 0xc3})
	require.NoError(t, err)
	_, err = w.Write([]byte{'c'})
	require.NoError(t, err)
	require.Equal(t, "a�b�c", strings.Join(*got, ""))
	require.Equal(t, "a\xffb\xc3c", dst.String())
}

func TestLiveWriterDropsAFragmentLeftAtTheEnd(t *testing.T) {
	w, dst, got := newLive()
	_, err := w.Write([]byte("ok\xe2\x82"))
	require.NoError(t, err)
	require.Equal(t, "ok", strings.Join(*got, ""))
	require.Equal(t, "ok\xe2\x82", dst.String())
}

func TestLiveWriterCapsWithOneMarker(t *testing.T) {
	w, dst, got := newLive()
	chunk := []byte(strings.Repeat("x", 600<<10))
	for range 4 {
		n, err := w.Write(chunk)
		require.NoError(t, err)
		require.Equal(t, len(chunk), n)
	}
	joined := strings.Join(*got, "")
	require.Equal(t, strings.Repeat("x", maxLiveOutputBytes)+liveCapMarker, joined)
	require.Equal(t, 1, strings.Count(joined, "live output stopped"))
	require.Contains(t, dst.String(), "bytes omitted")
}

func TestLiveWriterAtExactlyTheCapEmitsNoMarker(t *testing.T) {
	w, _, got := newLive()
	_, err := w.Write([]byte(strings.Repeat("x", maxLiveOutputBytes)))
	require.NoError(t, err)
	require.Len(t, strings.Join(*got, ""), maxLiveOutputBytes)
	_, err = w.Write(nil)
	require.NoError(t, err)
	_, err = w.Write([]byte{})
	require.NoError(t, err)
	require.Len(t, strings.Join(*got, ""), maxLiveOutputBytes)
}

type modelFunc func(context.Context, []durable.Message, []durable.ToolSpec) (durable.Message, error)

func (f modelFunc) Generate(ctx context.Context, h []durable.Message, t []durable.ToolSpec) (durable.Message, error) {
	return f(ctx, h, t)
}

type bashDriver struct {
	mu sync.Mutex
	ds []durable.Delta
}

func (b *bashDriver) add(d durable.Delta) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ds = append(b.ds, d)
}

// runBashThroughHarness is the only way to get a sink, so the test drives the real bash tool via a Harness.
func runBashThroughHarness(t *testing.T, command string) ([]durable.Delta, string) {
	t.Helper()
	ctx := context.Background()
	e := newEnv(t)
	input, err := json.Marshal(map[string]any{"command": command, "timeout_seconds": nil})
	require.NoError(t, err)
	model := modelFunc(func(_ context.Context, hist []durable.Message, _ []durable.ToolSpec) (durable.Message, error) {
		if hist[len(hist)-1].Role == durable.RoleUser {
			return durable.Message{Role: durable.RoleAssistant, ToolCalls: []durable.ToolCall{{ID: "c1", Name: "bash", Input: input}}}, nil
		}
		return durable.Message{Role: durable.RoleAssistant, Text: "done"}, nil
	})
	var b bashDriver
	h, err := durable.Open(ctx, filepath.Join(t.TempDir(), "s.sqlite"), durable.Options{
		Model: model, Tools: []durable.Tool{e.tools["bash"]}, OnDelta: b.add,
	})
	require.NoError(t, err)
	defer h.Close()
	_, err = h.Prompt(ctx, "a", "go")
	require.NoError(t, err)
	hist, err := h.History(ctx)
	require.NoError(t, err)
	for _, m := range hist {
		if m.Role == durable.RoleTool {
			return b.ds, m.Text
		}
	}
	t.Fatal("no tool result")
	return nil, ""
}

func TestBashEmitsInterleavedOutputAndLeavesTheResultUnchanged(t *testing.T) {
	ds, result := runBashThroughHarness(t, "echo out; echo err >&2; echo end")
	var live strings.Builder
	for _, d := range ds {
		if d.Kind == durable.DeltaReset {
			continue
		}
		require.Equal(t, durable.DeltaToolOutput, d.Kind)
		require.Equal(t, "c1", d.CallID)
		live.WriteString(d.Text)
	}
	require.Equal(t, "out\nerr\nend\n", live.String())
	require.Equal(t, "out\nerr\nend\n", result)
}

func TestBashLiveOutputPastTheResultCapStillMatchesPrefix(t *testing.T) {
	ds, result := runBashThroughHarness(t, `head -c 100000 /dev/zero | tr '\0' x`)
	var live strings.Builder
	for _, d := range ds {
		live.WriteString(d.Text)
	}
	require.Len(t, live.String(), 100000)
	require.Contains(t, result, "bytes omitted")
	require.Less(t, len(result), maxOutputBytes+100)
}
