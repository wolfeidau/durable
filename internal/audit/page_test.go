package audit_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/audit"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
)

func ids(events []audit.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func hosts(events []audit.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Host
	}
	return out
}

func TestReadRejectsCursor(t *testing.T) {
	q := query(fakeGateway(nil, nil), now)
	q.After = "anything"

	_, err := audit.Read(t.Context(), q)

	require.ErrorIs(t, err, audit.ErrBadCursor)
}

func gatewayLog(n int, at time.Time) []openshell.Decision {
	var ds []openshell.Decision
	for i := range n {
		ds = append(ds, openshell.Decision{Time: at.Add(time.Duration(i) * time.Second), Allowed: true, Kind: "HTTP", Host: fmt.Sprintf("h%d.com", i)})
	}
	return ds
}

func TestReadGatewayHonoursLimit(t *testing.T) {
	since := now.Add(-time.Hour)
	q := query(fakeGateway(gatewayLog(5, since.Add(time.Minute)), nil), since)
	q.Limit = 2

	res, err := audit.Read(t.Context(), q)

	require.NoError(t, err)
	require.Equal(t, []string{"h3.com", "h4.com"}, hosts(res.Events), "the newest events, oldest first")
	require.True(t, res.Truncated)
	require.NotEmpty(t, res.Events[0].ID)
}

func TestGatewayIDsAreDeterministicAndDistinguishDuplicates(t *testing.T) {
	since := now.Add(-time.Hour)
	at := since.Add(time.Minute)
	same := openshell.Decision{Time: at, Allowed: true, Kind: "HTTP", Host: "dup.com"}
	other := openshell.Decision{Time: at, Allowed: false, Kind: "HTTP", Host: "dup.com"}
	gw := func(context.Context, string, time.Time) ([]openshell.Decision, error) {
		return []openshell.Decision{same, other, same}, nil
	}

	a, err := audit.Read(t.Context(), query(gw, since))
	require.NoError(t, err)
	b, err := audit.Read(t.Context(), query(gw, since))
	require.NoError(t, err)

	require.Len(t, a.Events, 3)
	require.Equal(t, ids(a.Events), ids(b.Events))
	seen := map[string]bool{}
	for _, id := range ids(a.Events) {
		require.False(t, seen[id], "ids must be unique: %s", id)
		seen[id] = true
	}
}
