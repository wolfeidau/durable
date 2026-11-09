// Package sandbox runs durable tools on a worker inside a sandbox.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/wolfeidau/durable"
	workerv1 "github.com/wolfeidau/durable/api/durable/worker/v1"
	"github.com/wolfeidau/durable/api/durable/worker/v1/workerv1connect"
	"github.com/wolfeidau/durable/tools"
)

const (
	cancelTimeout = 3 * time.Second
	recoverWait   = 5 * time.Second
	recoverEvery  = 100 * time.Millisecond
)

// Client talks to one worker.
type Client struct {
	rpc  workerv1connect.ToolServiceClient
	http *http.Client

	recoverWait  time.Duration
	recoverEvery time.Duration
}

// DialFunc opens a connection to the worker, for transports that are not plain TCP.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// tokenAuth adds the bearer token to unary and streaming calls.
type tokenAuth struct{ token string }

var _ connect.Interceptor = tokenAuth{}

func (a tokenAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+a.token)
		return next(ctx, req)
	}
}

func (a tokenAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+a.token)
		return conn
	}
}

func (a tokenAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// NewClient returns a client for the worker at baseURL. A nil hc gets a cleartext HTTP/2 client. It makes no request.
func NewClient(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = h2cClient(nil)
	}
	return &Client{
		rpc:          workerv1connect.NewToolServiceClient(hc, baseURL, connect.WithInterceptors(tokenAuth{token})),
		http:         hc,
		recoverWait:  recoverWait,
		recoverEvery: recoverEvery,
	}
}

// NewDialClient is NewClient over connections opened by dial.
func NewDialClient(baseURL, token string, dial DialFunc) *Client {
	return NewClient(baseURL, token, h2cClient(dial))
}

func h2cClient(dial DialFunc) *http.Client {
	transport := &http.Transport{Protocols: new(http.Protocols), DialContext: dial}
	transport.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: transport}
}

// CloseIdle closes idle connections to the worker.
func (c *Client) CloseIdle() { c.http.CloseIdleConnections() }

// Tools returns remote tools built from the server's catalog. It fails unless the worker offers exactly the
// catalog's tools in order with the same descriptions and schemas, so a session never opens against a mismatched worker.
func (c *Client) Tools(ctx context.Context) ([]durable.Tool, error) {
	res, err := c.rpc.ListTools(ctx, connect.NewRequest(&workerv1.ListToolsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("failed to list worker tools: %w", err)
	}
	catalog := tools.Catalog()
	got := res.Msg.Tools
	if len(got) != len(catalog) {
		return nil, fmt.Errorf("worker offers %d tools %v, want %d %v", len(got), toolNames(got), len(catalog), catalogNames(catalog))
	}
	out := make([]durable.Tool, 0, len(catalog))
	for i, e := range catalog {
		info := got[i]
		if info.Name != e.Spec.Name {
			return nil, fmt.Errorf("worker tool %d is %q, want %q (worker offers %v)", i, info.Name, e.Spec.Name, toolNames(got))
		}
		if info.Description != e.Spec.Description {
			return nil, fmt.Errorf("worker tool %q has a different description than the server's", info.Name)
		}
		same, err := sameJSON(info.InputSchema, e.Spec.Schema)
		if err != nil {
			return nil, fmt.Errorf("worker tool %q has an invalid schema: %w", info.Name, err)
		}
		if !same {
			return nil, fmt.Errorf("worker tool %q has a different input schema than the server's", info.Name)
		}
		out = append(out, &remoteTool{c: c, spec: e.Spec, readOnly: e.ReadOnly, replay: e.Replay})
	}
	return out, nil
}

func toolNames(infos []*workerv1.ToolInfo) []string {
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	return names
}

func catalogNames(entries []tools.Entry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Spec.Name)
	}
	return names
}

// sameJSON compares canonical forms, because a worker may re-encode a schema without changing it.
func sameJSON(a, b []byte) (bool, error) {
	ca, err := canonical(a)
	if err != nil {
		return false, err
	}
	cb, err := canonical(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ca, cb), nil
}

func canonical(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
