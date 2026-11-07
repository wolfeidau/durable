package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
)

const testToken = "test-token"

func (e *registryEnv) watchClient(t *testing.T) (durablev1connect.SessionServiceClient, *Registry) {
	t.Helper()
	r := e.registry(t)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = New(Config{Home: e.home, Token: testToken, Addr: ts.Listener.Addr().String(), Registry: r}).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return durablev1connect.NewSessionServiceClient(http.DefaultClient, ts.URL), r
}

func watchReq(id string) *connect.Request[v1.WatchEventsRequest] {
	req := connect.NewRequest(&v1.WatchEventsRequest{SessionId: id})
	req.Header().Set("Authorization", "Bearer "+testToken)
	return req
}

func TestWatchMapsOpenFailureToInternalWithoutPath(t *testing.T) {
	e := newEnv(t)
	client, _ := e.watchClient(t)

	// A session directory whose metadata is unreadable fails to open for a reason that isn't the client's.
	id := "0123456789abcdef"
	dir := filepath.Join(e.home, ".durable", "sessions", id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.sqlite"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.json"), []byte("{"), 0o600))

	stream, err := client.WatchEvents(t.Context(), watchReq(id))
	require.NoError(t, err)
	require.False(t, stream.Receive())
	require.Equal(t, connect.CodeInternal, connect.CodeOf(stream.Err()), "%v", stream.Err())
	require.Contains(t, stream.Err().Error(), "internal error")
	require.NotContains(t, stream.Err().Error(), e.home)
	require.NotContains(t, stream.Err().Error(), "session.json")
}

func TestWatchPagesThroughALongLog(t *testing.T) {
	old := watchPage
	watchPage = 3
	t.Cleanup(func() { watchPage = old })

	e := newEnv(t)
	client, reg := e.watchClient(t)
	s, release, err := reg.Acquire(t.Context(), e.id)
	require.NoError(t, err)
	defer release()
	for i := range 10 {
		_, err := s.Harness().Submit(t.Context(), "req-"+strconv.Itoa(i), "go")
		require.NoError(t, err)
	}
	want := s.newestSeq()
	require.Greater(t, want, int64(2*watchPage))

	stream, err := client.WatchEvents(t.Context(), watchReq(e.id))
	require.NoError(t, err)
	defer stream.Close()
	var got []int64
	for int64(len(got)) < want {
		require.True(t, stream.Receive(), "%v", stream.Err())
		if ev := stream.Msg().GetEvent(); ev != nil {
			got = append(got, ev.GetSeq())
		}
	}
	for i, seq := range got {
		require.Equal(t, int64(i+1), seq)
	}
}

func TestWatchReturnsHeadersBeforeTheFirstEvent(t *testing.T) {
	e := newEnv(t)
	client, _ := e.watchClient(t)
	start := time.Now()
	stream, err := client.WatchEvents(t.Context(), watchReq(e.id))
	require.NoError(t, err)
	defer stream.Close()
	// The session has no events and the heartbeat is 20s, so only the early headers return the call this fast.
	require.Less(t, time.Since(start), 5*time.Second)
}
