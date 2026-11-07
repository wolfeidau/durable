package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	v1 "github.com/wolfeidau/durable/api/durable/v1"
	"github.com/wolfeidau/durable/api/durable/v1/durablev1connect"
)

type faultConn struct{ client, up *net.TCPConn }

func (c *faultConn) reset() {
	_ = c.client.SetLinger(0)
	_ = c.client.Close()
	_ = c.up.Close()
}

// cut ends the client's read side with a clean FIN, so its response body stops early.
func (c *faultConn) cut() {
	_ = c.client.CloseWrite()
	_ = c.up.Close()
}

// faultProxy forwards loopback TCP to upstream and injects faults on the downstream (server to client) path.
type faultProxy struct {
	ln       net.Listener
	upstream string
	latency  atomic.Int64 // per downstream chunk, in nanoseconds
	resetAt  atomic.Int64 // downstream byte count that triggers a reset, 0 for off
	cutAt    atomic.Int64 // downstream byte count that triggers a cut, 0 for off
	faulty   atomic.Int64 // connections still subject to the byte triggers
	mu       sync.Mutex
	conns    []*faultConn
	wg       sync.WaitGroup
}

func newFaultProxy(t *testing.T, r *running) *faultProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &faultProxy{ln: ln, upstream: strings.TrimPrefix(r.url, "http://")}
	p.wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.wg.Go(func() { p.serve(c.(*net.TCPConn)) })
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		p.each((*faultConn).reset)
		p.wg.Wait()
	})
	return p
}

func (p *faultProxy) each(fn func(*faultConn)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		fn(c)
	}
}

// Reset aborts every open connection; Cut closes them cleanly.
func (p *faultProxy) Reset() { p.each((*faultConn).reset) }
func (p *faultProxy) Cut()   { p.each((*faultConn).cut) }

func (p *faultProxy) serve(client *net.TCPConn) {
	upConn, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	fc := &faultConn{client: client, up: upConn.(*net.TCPConn)}
	p.mu.Lock()
	p.conns = append(p.conns, fc)
	p.mu.Unlock()
	defer func() { _ = client.Close(); _ = fc.up.Close() }()

	faulted := p.faulty.Add(-1) >= 0
	go func() { _, _ = io.Copy(fc.up, client); _ = fc.up.CloseWrite() }()

	buf := make([]byte, 32<<10)
	var sent int64
	for {
		n, err := fc.up.Read(buf)
		if n > 0 {
			time.Sleep(time.Duration(p.latency.Load()))
			chunk := buf[:n]
			reset, cut := p.resetAt.Load(), p.cutAt.Load()
			limit := max(reset, cut)
			if faulted && limit > 0 && sent+int64(n) >= limit {
				_, _ = client.Write(chunk[:max(limit-sent, 0)])
				if reset > 0 {
					fc.reset()
				} else {
					fc.cut()
				}
				return
			}
			if _, werr := client.Write(chunk); werr != nil {
				return
			}
			sent += int64(n)
		}
		if err != nil {
			_ = client.CloseWrite()
			return
		}
	}
}

// hostRewriter sends the real server's Host, which the server's DNS rebinding check requires.
type hostRewriter struct {
	next http.RoundTripper
	host string
}

func (h hostRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Host = h.host
	return h.next.RoundTrip(req)
}

func faultClient(r *running, p *faultProxy, opts ...connect.ClientOption) durablev1connect.SessionServiceClient {
	return faultClientOver(h2cClient(), r, p, opts...)
}

// faultClientOver sends through p over hc, so a test can choose HTTP/2 or HTTP/1.1.
func faultClientOver(hc *http.Client, r *running, p *faultProxy, opts ...connect.ClientOption) durablev1connect.SessionServiceClient {
	hc.Transport = hostRewriter{next: hc.Transport, host: strings.TrimPrefix(r.url, "http://")}
	return durablev1connect.NewSessionServiceClient(hc, "http://"+p.ln.Addr().String(), opts...)
}

type faultResult struct {
	w          *watcher
	errs       int // attempts that ended with an error
	cleanEnds  int // attempts that ended with no error before stop
	attempts   int
	firstErrAt int // events applied when the first error ended an attempt, or -1
}

// faultAttempt streams once and returns the error that ended it; nil before stop means a clean end.
func faultAttempt(client durablev1connect.SessionServiceClient, w *watcher, stop func(*watcher) bool, onItem func(*watcher)) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := client.WatchEvents(ctx, w.request(w.epoch(), w.last()))
	if err != nil {
		return err
	}
	defer stream.Close()
	for !stop(w) && stream.Receive() {
		switch item := stream.Msg().GetItem().(type) {
		case *v1.WatchEventsResponse_Event:
			w.events = append(w.events, item.Event)
		case *v1.WatchEventsResponse_Heartbeat:
			w.heartbeats++
		}
		if onItem != nil {
			onItem(w)
		}
	}
	if stop(w) {
		return nil
	}
	return stream.Err()
}

// faultWatch follows the session with a resume loop until stop is true, calling onItem after each message.
func faultWatch(t *testing.T, client durablev1connect.SessionServiceClient, r *running, id string, stop func(*watcher) bool, onItem func(*watcher)) *faultResult {
	t.Helper()
	res := &faultResult{w: &watcher{client: client, token: r.token, id: id}, firstErrAt: -1}
	w := res.w
	deadline := time.Now().Add(watchTimeout)
	for !stop(w) {
		require.True(t, time.Now().Before(deadline), "no progress; seqs: %v", seqs(w.events))
		res.attempts++
		err := faultAttempt(client, w, stop, onItem)
		switch {
		case stop(w):
		case err == nil:
			res.cleanEnds++
		default:
			if res.errs == 0 {
				res.firstErrAt = len(w.events)
			}
			res.errs++
			if connect.CodeOf(err) == connect.CodeFailedPrecondition {
				w.events = nil
			}
		}
	}
	return res
}

// faultFinal reads the whole log with one clean watch straight from the server.
func faultFinal(t *testing.T, r *running, id string, n int, opts ...connect.ClientOption) []int64 {
	t.Helper()
	w := newWatcher(r, id, opts...)
	w.followUntilDone(t, n)
	return seqs(w.events)
}

// faultTransports pairs each protocol with its transports. gRPC needs HTTP/2, and browsers stream over HTTP/1.1.
var faultTransports = []struct {
	name string
	h1   bool
	opts []connect.ClientOption
}{
	{"connect/h2", false, nil},
	{"grpc/h2", false, []connect.ClientOption{connect.WithGRPC()}},
	{"grpc-web/h2", false, []connect.ClientOption{connect.WithGRPCWeb()}},
	{"connect/h1", true, nil},
	{"grpc-web/h1", true, []connect.ClientOption{connect.WithGRPCWeb()}},
}

func TestFaultLatencyDelaysButLosesNothing(t *testing.T) {
	_, home, id, _ := seedSession(t, textReply("hi"), 100*time.Millisecond, 3)
	r := startIn(t, home)
	p := newFaultProxy(t, r)
	p.latency.Store(int64(75 * time.Millisecond))

	res := faultWatch(t, faultClient(r, p), r, id, func(w *watcher) bool { return w.done() >= 3 }, nil)
	require.Equal(t, 1, res.attempts)
	requireContiguous(t, res.w.events)
	require.Equal(t, faultFinal(t, r, id, 3), seqs(res.w.events))
}

func TestFaultMidStreamBreakResumesExactly(t *testing.T) {
	for _, mode := range []string{"reset", "cut"} {
		for _, tr := range faultTransports {
			t.Run(mode+"/"+tr.name, func(t *testing.T) {
				_, home, id, _ := seedSession(t, textReply("hi"), 150*time.Millisecond, 4)
				r := startIn(t, home)
				p := newFaultProxy(t, r)
				p.faulty.Store(3)
				if mode == "reset" {
					p.resetAt.Store(600)
				} else {
					p.cutAt.Store(600)
				}

				hc := h2cClient()
				if tr.h1 {
					hc = &http.Client{Transport: &http.Transport{}}
				}
				res := faultWatch(t, faultClientOver(hc, r, p, tr.opts...), r, id, func(w *watcher) bool { return w.done() >= 4 }, nil)
				t.Logf("%s/%s: attempts=%d errors=%d cleanEnds=%d firstErrAt=%d", mode, tr.name, res.attempts, res.errs, res.cleanEnds, res.firstErrAt)
				require.Positive(t, res.errs+res.cleanEnds, "the fault never hit")
				require.Zero(t, res.cleanEnds, "a truncated stream ended with no error")
				final := faultFinal(t, r, id, 4, tr.opts...)
				// Some events arrived before the first break and more after it, so it hit mid-stream.
				require.Positive(t, res.firstErrAt, "the fault hit before any event")
				require.Less(t, res.firstErrAt, len(final), "the fault hit after the last event")
				requireContiguous(t, res.w.events)
				require.Equal(t, final, seqs(res.w.events))
			})
		}
	}
}

func TestFaultIdleCutDuringHeartbeatWait(t *testing.T) {
	for _, mode := range []string{"reset", "cut"} {
		t.Run(mode, func(t *testing.T) {
			_, home, id, _ := seedSession(t, textReply("hi"), 0, 1)
			r := startIn(t, home, "--heartbeat", "200ms")
			p := newFaultProxy(t, r)
			client := newClient(r)

			var broke bool
			onItem := func(w *watcher) {
				if broke || w.done() < 1 || w.heartbeats < 1 {
					return
				}
				broke = true
				if mode == "reset" {
					p.Reset()
				} else {
					p.Cut()
				}
				_, err := client.submit(t, id, "later")
				require.NoError(t, err)
			}
			res := faultWatch(t, faultClient(r, p), r, id, func(w *watcher) bool { return w.done() >= 2 }, onItem)
			require.True(t, broke)
			require.Positive(t, res.errs)
			require.Zero(t, res.cleanEnds)
			requireContiguous(t, res.w.events)
			require.Equal(t, faultFinal(t, r, id, 2), seqs(res.w.events))
		})
	}
}
