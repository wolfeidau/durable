package audit_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/audit"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
	"github.com/wolfeidau/durable/internal/telemetry"
)

func ids(events []audit.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func seedStore(t *testing.T, since time.Time, uids ...string) *telemetry.Store {
	t.Helper()
	s := openStore(t)
	var evs []telemetry.Event
	for i, uid := range uids {
		evs = append(evs, decisionEvent(uid, sandboxName, "id1", since.Add(time.Duration(i/2)*time.Second), uid+".com"))
	}
	add(t, s, evs...)
	return s
}

func TestReadStoreCursorRoundTrip(t *testing.T) {
	since := now.Add(-time.Hour)
	s := seedStore(t, since, "a", "b", "c", "d", "e")
	q := query(s, nil, since)
	q.Limit = 2

	first, err := audit.Read(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, []string{"d", "e"}, ids(first.Events), "the newest events, oldest first")
	require.True(t, first.Truncated, "older events were left out")
	require.NotEmpty(t, first.NextCursor)

	add(t, s, decisionEvent("f", sandboxName, "id1", since.Add(time.Minute), "f.com"), decisionEvent("g", sandboxName, "id1", since.Add(time.Minute), "g.com"), decisionEvent("h", sandboxName, "id1", since.Add(time.Minute), "h.com"))
	q.After = first.NextCursor
	next, err := audit.Read(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, []string{"f", "g"}, ids(next.Events))
	require.True(t, next.Truncated, "newer events remain")
	require.Equal(t, audit.SourceStore, next.Coverage.Source)

	q.After = next.NextCursor
	last, err := audit.Read(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, []string{"h"}, ids(last.Events))
	require.False(t, last.Truncated)

	q.After = last.NextCursor
	none, err := audit.Read(t.Context(), q)
	require.NoError(t, err)
	require.Empty(t, none.Events)
	require.Equal(t, last.NextCursor, none.NextCursor, "an empty poll keeps the cursor")
	require.Equal(t, audit.SourceStore, none.Coverage.Source, "a store cursor never falls back to the gateway")
	require.False(t, none.Coverage.StoreEmpty)
}

func TestReadStoreCursorNeedsStore(t *testing.T) {
	since := now.Add(-time.Hour)
	res, err := audit.Read(t.Context(), func() audit.Query {
		q := query(seedStore(t, since, "a"), nil, since)
		q.Limit = 1
		return q
	}())
	require.NoError(t, err)

	q := query(nil, nil, since)
	q.After = res.NextCursor
	_, err = audit.Read(t.Context(), q)

	require.ErrorIs(t, err, audit.ErrBadCursor)
}

func TestReadRejectsMalformedCursor(t *testing.T) {
	for _, c := range []string{"!!!", "bm90LWEtY3Vyc29y", "eDoxOmE"} {
		q := query(nil, nil, now)
		q.After = c
		_, err := audit.Read(t.Context(), q)
		require.ErrorIs(t, err, audit.ErrBadCursor, c)
	}
}

func gatewayLog(n int, at time.Time) []openshell.Decision {
	var ds []openshell.Decision
	for i := range n {
		ds = append(ds, openshell.Decision{Time: at.Add(time.Duration(i) * time.Second), Allowed: true, Kind: "HTTP", Host: fmt.Sprintf("h%d.com", i)})
	}
	return ds
}

func TestReadGatewayPaging(t *testing.T) {
	since := now.Add(-time.Hour)
	log := gatewayLog(5, since.Add(time.Minute))
	gw := func(context.Context, string, time.Time) ([]openshell.Decision, error) { return log, nil }
	q := query(nil, gw, since)
	q.Limit = 2

	first, err := audit.Read(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, audit.SourceGateway, first.Coverage.Source)
	require.Equal(t, []string{"h3.com", "h4.com"}, hosts(first.Events))
	require.True(t, first.Truncated)
	require.NotEmpty(t, first.Events[0].ID)

	q.After = first.NextCursor
	next, err := audit.Read(t.Context(), q)
	require.NoError(t, err)
	require.Empty(t, next.Events)
	require.False(t, next.Truncated)
	require.Equal(t, first.NextCursor, next.NextCursor)

	log = append(log, gatewayLog(8, since.Add(time.Minute))[5:]...)
	// A cursor from the log reads the log again even though a store is configured.
	q.Store = seedStore(t, since, "a")
	res, err := audit.Read(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, audit.SourceGateway, res.Coverage.Source)
	require.False(t, res.Coverage.StoreEmpty, "the store was not consulted for events")
	require.Equal(t, []string{"h5.com", "h6.com"}, hosts(res.Events))
	require.True(t, res.Truncated, "newer events remain")
}

func hosts(events []audit.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Host
	}
	return out
}

func TestGatewayIDsAreDeterministicAndDistinguishDuplicates(t *testing.T) {
	since := now.Add(-time.Hour)
	at := since.Add(time.Minute)
	same := openshell.Decision{Time: at, Allowed: true, Kind: "HTTP", Host: "dup.com"}
	other := openshell.Decision{Time: at, Allowed: false, Kind: "HTTP", Host: "dup.com"}
	gw, _ := fakeGateway([]openshell.Decision{same, other, same}, nil)

	a, err := audit.Read(t.Context(), query(nil, gw, since))
	require.NoError(t, err)
	b, err := audit.Read(t.Context(), query(nil, gw, since))
	require.NoError(t, err)

	require.Len(t, a.Events, 3)
	require.Equal(t, ids(a.Events), ids(b.Events))
	seen := map[string]bool{}
	for _, id := range ids(a.Events) {
		require.False(t, seen[id], "ids must be unique: %s", id)
		seen[id] = true
	}
}
