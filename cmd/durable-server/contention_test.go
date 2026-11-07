package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/wolfeidau/durable"
	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
)

const (
	// Large submissions make the event log several MB, so a stalled reader fills the socket buffers.
	contentionSubmissions = 6
	contentionTextBytes   = 1 << 20
	contentionProbes      = 5
	// A probe is a tiny submission, so seeing it takes milliseconds; this bound is a stall detector, not a benchmark.
	contentionProbeBound = 3 * time.Second
)

// contentionFeed records what a reading watcher received.
type contentionFeed struct {
	seqs  []int64
	bytes int
	done  int
	// queued maps a submission seq to when its QUEUED event arrived.
	queued map[int64]time.Time
}

// contentionWatch streams from 0 until it has seen want finished submissions.
func contentionWatch(ctx context.Context, r *running, id string, want int) (*contentionFeed, error) {
	client := durablev1connect.NewSessionServiceClient(h2cClient(), r.url)
	req := connect.NewRequest(&v1.WatchEventsRequest{SessionId: id})
	req.Header().Set("Authorization", "Bearer "+r.token)
	stream, err := client.WatchEvents(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	f := &contentionFeed{queued: map[int64]time.Time{}}
	for f.done < want {
		if !stream.Receive() {
			return f, fmt.Errorf("stream ended early: %w", stream.Err())
		}
		e := stream.Msg().GetEvent()
		if e == nil {
			continue
		}
		f.seqs = append(f.seqs, e.GetSeq())
		f.bytes += proto.Size(e)
		if e.GetKind() != v1.EventKind_EVENT_KIND_SUBMISSION {
			continue
		}
		if e.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_DONE {
			f.done++
		}
		if e.GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_QUEUED {
			f.queued[e.GetSubmission()] = time.Now()
		}
	}
	return f, nil
}

// contentionStall opens WatchEvents over raw HTTP/1.1 with a tiny receive buffer and doesn't read until the caller does.
func contentionStall(t *testing.T, r *running, id string) net.Conn {
	t.Helper()
	addr := strings.TrimPrefix(r.url, "http://")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(4096))

	msg := fmt.Sprintf(`{"sessionId":%q}`, id)
	body := make([]byte, 5, 5+len(msg))
	binary.BigEndian.PutUint32(body[1:], uint32(len(msg)))
	body = append(body, msg...)
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/connect+json\r\nConnect-Protocol-Version: 1\r\nContent-Length: %d\r\n\r\n",
		durablev1connect.SessionServiceWatchEventsProcedure, addr, r.token, len(body))
	_, err = conn.Write(append([]byte(head), body...))
	require.NoError(t, err)

	return conn
}

// contentionRun runs one scripted session with the given watchers and returns the Run wall time.
func contentionRun(t *testing.T, watchers int, stalled bool) time.Duration {
	t.Helper()
	ws, home := newWorkspace(t)
	id, dbPath := createSessionDB(t, ws, home)
	api := newFakeAPI(t, textReply("ok"), 20*time.Millisecond)
	r := startEnv(t, home, apiEnv(api), "--api-token", "dummy", "--idle-timeout", "1s")
	c := newClient(r)

	ctx, cancel := context.WithTimeout(t.Context(), 2*testTimeout)
	defer cancel()
	total := contentionSubmissions + contentionProbes

	var stalledConn net.Conn
	if stalled {
		stalledConn = contentionStall(t, r, id)
	}
	feeds := make([]*contentionFeed, watchers)
	errs := make([]error, watchers)
	var wg sync.WaitGroup
	for i := range watchers {
		wg.Go(func() { feeds[i], errs[i] = contentionWatch(ctx, r, id, total) })
	}
	// Let the readers attach before the run starts.
	time.Sleep(300 * time.Millisecond)

	submit := func(reqID, text string) *v1.Submission {
		resp, err := c.Submit(ctx, authed(c, &v1.SubmitRequest{SessionId: id, RequestId: reqID, Text: text}))
		require.NoError(t, err)
		return resp.Msg.GetSubmission()
	}
	waitDone := func(reqID, text string) {
		require.Eventually(t, func() bool {
			return submit(reqID, text).GetStatus() == v1.SubmissionStatus_SUBMISSION_STATUS_DONE
		}, testTimeout, 50*time.Millisecond)
	}

	big := strings.Repeat("x", contentionTextBytes)
	start := time.Now()
	for i := range contentionSubmissions {
		submit(fmt.Sprintf("big-%d", i), big)
	}
	// Submissions run in order, so the last one done means all are.
	waitDone(fmt.Sprintf("big-%d", contentionSubmissions-1), big)
	wall := time.Since(start)

	// A probe's commit time is approximated by its Submit return; the reader can't see it earlier.
	returned := map[int64]time.Time{}
	for i := range contentionProbes {
		reqID := fmt.Sprintf("probe-%d", i)
		returned[submit(reqID, "ping").GetSeq()] = time.Now()
		waitDone(reqID, "ping")
	}

	wg.Wait()
	require.NoError(t, errors.Join(errs...))

	if watchers > 0 {
		ref, err := contentionWatch(ctx, r, id, total)
		require.NoError(t, err)
		t.Logf("watchers=%d stalled=%v: %d events, %d KB", watchers, stalled, len(ref.seqs), ref.bytes>>10)
		require.Greater(t, ref.bytes, 4<<20, "the log must outgrow the socket buffers")
		for i, seq := range ref.seqs {
			require.Equal(t, int64(i+1), seq, "reference seqs have a gap")
		}
		for i, f := range feeds {
			require.Equal(t, ref.seqs, f.seqs, "watcher %d", i)
		}
		var worst time.Duration
		for seq, ret := range returned {
			arrived, ok := feeds[0].queued[seq]
			require.True(t, ok, "probe submission %d not seen by the reading watcher", seq)
			worst = max(worst, arrived.Sub(ret))
		}
		t.Logf("probe latency, worst of %d (arrival minus Submit return): %v", contentionProbes, worst)
		require.Less(t, worst, contentionProbeBound)
	}

	if stalled {
		// Reading the status line now proves the stream opened and had data, then close it.
		require.NoError(t, stalledConn.SetReadDeadline(time.Now().Add(10*time.Second)))
		line, err := bufio.NewReader(stalledConn).ReadString('\n')
		require.NoError(t, err)
		require.Contains(t, line, "200")
		require.NoError(t, stalledConn.Close())
	}
	// The server still serves a new watch, and the session idle-closes once no stream holds it.
	again, err := contentionWatch(ctx, r, id, total)
	require.NoError(t, err)
	require.Equal(t, total, again.done)
	require.Eventually(t, func() bool {
		probe, cancelProbe := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancelProbe()
		h, err := durable.Open(probe, dbPath, durable.Options{Model: idleModel{}})
		if err != nil {
			return false
		}
		_ = h.Close()
		return true
	}, 20*time.Second, 100*time.Millisecond, "session did not idle-close after the stalled watcher left")
	return wall
}

// createSessionDB creates the session and returns its ID and database path.
func createSessionDB(t *testing.T, ws, home string) (id, dbPath string) {
	t.Helper()
	createSession(t, ws, home, "s")
	return sessionDB(t, home)
}

func TestContentionRunTimeWithWatchers(t *testing.T) {
	baseline := contentionRun(t, 0, false)
	t.Logf("baseline (0 watchers): %v", baseline)

	for _, watchers := range []int{10, 50} {
		t.Run(fmt.Sprintf("%d watchers plus one stalled", watchers), func(t *testing.T) {
			wall := contentionRun(t, watchers, true)
			t.Logf("wall: %v with a stalled watcher, %v at 0 watchers", wall, baseline)
			// Fan-out is CPU bound (each reader gets the whole log), so the bound grows with readers; the slack covers -race on CI.
			require.Less(t, wall, 3*baseline+time.Duration(watchers)*250*time.Millisecond+2*time.Second)
			if watchers < 50 {
				return
			}
			// The same readers without the stalled one separate its cost from the fan-out cost.
			plain := contentionRun(t, watchers, false)
			t.Logf("wall: %v without the stalled watcher", plain)
			require.Less(t, wall, 2*plain+3*time.Second, "the stalled watcher slowed Run")
		})
	}
}
