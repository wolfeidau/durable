package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
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

// contentionWatch streams from 0 until it has seen want finished submissions. attached, if set, runs once the call returns.
func contentionWatch(ctx context.Context, r *running, id string, want int, attached func()) (*contentionFeed, error) {
	client := durablev1connect.NewSessionServiceClient(h2cClient(), r.url)
	req := connect.NewRequest(&v1.WatchEventsRequest{SessionId: id})
	req.Header().Set("Authorization", "Bearer "+r.token)
	stream, err := client.WatchEvents(ctx, req)
	if attached != nil {
		attached()
	}
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

// contentionStall opens WatchEvents over raw HTTP/1.1 with a tiny receive buffer, reads only the headers, and leaves the body unread.
func contentionStall(t *testing.T, r *running, id string) (net.Conn, *bufio.Reader) {
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

	// The server sends headers once the watch holds the session, so this proves it attached before the run.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, "200")
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	return conn, br
}

// contentionFirstEvent reads the stalled stream's body after the run and returns the first event's seq.
func contentionFirstEvent(t *testing.T, conn net.Conn, br *bufio.Reader) int64 {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	// The status line is already consumed, so read the rest of the response from a reconstructed head.
	resp, err := http.ReadResponse(bufio.NewReader(io.MultiReader(strings.NewReader("HTTP/1.1 200 OK\r\n"), br)), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	prefix := make([]byte, 5)
	_, err = io.ReadFull(resp.Body, prefix)
	require.NoError(t, err)
	msg := make([]byte, binary.BigEndian.Uint32(prefix[1:]))
	_, err = io.ReadFull(resp.Body, msg)
	require.NoError(t, err)
	var item v1.WatchEventsResponse
	require.NoError(t, protojson.Unmarshal(msg, &item))
	return item.GetEvent().GetSeq()
}

// contentionStatus reads a submission's status through a separate read-only connection, so polling adds no load to the server.
func contentionStatus(dbPath, reqID string) (string, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var status string
	err = db.QueryRow(`SELECT status FROM submissions WHERE request_id = ?`, reqID).Scan(&status)
	return status, err
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
	var stalledBody *bufio.Reader
	if stalled {
		stalledConn, stalledBody = contentionStall(t, r, id)
	}
	feeds := make([]*contentionFeed, watchers)
	errs := make([]error, watchers)
	var wg, attached sync.WaitGroup
	attached.Add(watchers)
	for i := range watchers {
		wg.Go(func() { feeds[i], errs[i] = contentionWatch(ctx, r, id, total, attached.Done) })
	}
	// Every reader holds the session before the run starts.
	attached.Wait()

	submit := func(reqID, text string) *v1.Submission {
		resp, err := c.Submit(ctx, authed(c, &v1.SubmitRequest{SessionId: id, RequestId: reqID, Text: text}))
		require.NoError(t, err)
		return resp.Msg.GetSubmission()
	}
	// Polls on the test goroutine, so a failure reports the last status and error.
	waitDone := func(reqID string) {
		deadline := time.Now().Add(testTimeout)
		for {
			status, err := contentionStatus(dbPath, reqID)
			if err == nil && status == string(durable.StatusDone) {
				return
			}
			require.True(t, time.Now().Before(deadline), "%s not done: status %q, error %v", reqID, status, err)
			time.Sleep(50 * time.Millisecond)
		}
	}

	big := strings.Repeat("x", contentionTextBytes)
	start := time.Now()
	for i := range contentionSubmissions {
		submit(fmt.Sprintf("big-%d", i), big)
	}
	// Submissions run in order, so the last one done means all are.
	waitDone(fmt.Sprintf("big-%d", contentionSubmissions-1))
	wall := time.Since(start)

	// A probe's commit time is approximated by its Submit return; the reader can't see it earlier.
	returned := map[int64]time.Time{}
	for i := range contentionProbes {
		reqID := fmt.Sprintf("probe-%d", i)
		returned[submit(reqID, "ping").GetSeq()] = time.Now()
		waitDone(reqID)
	}

	wg.Wait()
	require.NoError(t, errors.Join(errs...))

	if watchers > 0 {
		ref, err := contentionWatch(ctx, r, id, total, nil)
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
		// The backlog starts at seq 1, so the stalled stream was live and held the whole log behind it.
		require.Equal(t, int64(1), contentionFirstEvent(t, stalledConn, stalledBody))
		require.NoError(t, stalledConn.Close())
	}
	// The server still serves a new watch, and the session idle-closes once no stream holds it.
	again, err := contentionWatch(ctx, r, id, total, nil)
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
			if watchers >= 50 && testing.Short() {
				t.Skip("about 24s under -race; the 10-watcher case covers the stall in short mode")
			}
			wall := contentionRun(t, watchers, true)
			t.Logf("wall: %v with a stalled watcher, %v at 0 watchers", wall, baseline)
			// Only a hang guard: fan-out throughput swings with CI load, so stalls are judged by probe latency and the comparator below.
			require.Less(t, wall, 3*baseline+time.Duration(watchers)*time.Second+10*time.Second)
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
