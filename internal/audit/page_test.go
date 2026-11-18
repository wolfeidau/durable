package audit_test

import (
	"context"
	"encoding/base64"
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

func hosts(events []audit.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Host
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
	require.False(t, first.Reset)
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

func TestReadResumeReturnsLateArrivals(t *testing.T) {
	since := now.Add(-2 * time.Hour)
	s := seedStore(t, now.Add(-time.Hour), "a", "b")
	q := query(s, nil, since)
	first, err := audit.Read(t.Context(), q)
	require.NoError(t, err)

	// A retried export delivers an event older than rows the client already has.
	add(t, s, decisionEvent("late", sandboxName, "id1", now.Add(-90*time.Minute), "late.com"))
	q.After = first.NextCursor
	next, err := audit.Read(t.Context(), q)

	require.NoError(t, err)
	require.Equal(t, []string{"late"}, ids(next.Events))
	require.Less(t, next.Events[0].Time, first.Events[0].Time)
}

func TestReadFirstLoadCursorCoversEverythingStored(t *testing.T) {
	since := now.Add(-time.Hour)
	s := seedStore(t, since, "a", "b", "c", "d", "e", "f")
	q := query(s, nil, since)
	q.Limit = 2
	first, err := audit.Read(t.Context(), q)
	require.NoError(t, err)
	require.True(t, first.Truncated)

	q.After = first.NextCursor
	next, err := audit.Read(t.Context(), q)

	require.NoError(t, err)
	require.Empty(t, next.Events, "rows older than the first page are not delivered later")
	require.False(t, next.Truncated)
}

func TestReadCursorBeyondStoreResets(t *testing.T) {
	since := now.Add(-time.Hour)
	s := seedStore(t, since, "a", "b", "c")
	q := query(s, nil, since)
	first, err := audit.Read(t.Context(), q)
	require.NoError(t, err)

	// A fresh store has no row as high as the cursor.
	fresh := seedStore(t, since, "x")
	q = query(fresh, nil, since)
	q.After = first.NextCursor
	res, err := audit.Read(t.Context(), q)

	require.NoError(t, err)
	require.True(t, res.Reset)
	require.Equal(t, []string{"x"}, ids(res.Events))
	require.NotEmpty(t, res.NextCursor)
	require.Equal(t, audit.SourceStore, res.Coverage.Source)
}

func TestReadResetOnEmptyStoreFallsBackToGateway(t *testing.T) {
	since := now.Add(-time.Hour)
	first, err := audit.Read(t.Context(), func() audit.Query { return query(seedStore(t, since, "a", "b", "c"), nil, since) }())
	require.NoError(t, err)
	gw, _ := fakeGateway(gatewayLog(1, since.Add(time.Minute)), nil)
	q := query(openStore(t), gw, since)
	q.After = first.NextCursor

	res, err := audit.Read(t.Context(), q)

	require.NoError(t, err)
	require.True(t, res.Reset)
	require.Equal(t, audit.SourceGateway, res.Coverage.Source)
	require.Empty(t, res.NextCursor)
}

func TestReadStoreCursorNeedsStore(t *testing.T) {
	since := now.Add(-time.Hour)
	res, err := audit.Read(t.Context(), query(seedStore(t, since, "a"), nil, since))
	require.NoError(t, err)

	q := query(nil, nil, since)
	q.After = res.NextCursor
	_, err = audit.Read(t.Context(), q)

	require.ErrorIs(t, err, audit.ErrBadCursor)
}

func TestReadRejectsMalformedCursor(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, c := range []string{"!!!", enc("not-a-cursor"), enc("s:abc"), enc("s:0"), enc("s:-4"), enc("g:1:abc"), enc("x:1")} {
		q := query(openStore(t), nil, now)
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

func TestReadGatewayIssuesNoCursorAndHonoursLimit(t *testing.T) {
	since := now.Add(-time.Hour)
	gw, _ := fakeGateway(gatewayLog(5, since.Add(time.Minute)), nil)
	q := query(nil, gw, since)
	q.Limit = 2

	res, err := audit.Read(t.Context(), q)

	require.NoError(t, err)
	require.Equal(t, audit.SourceGateway, res.Coverage.Source)
	require.Equal(t, []string{"h3.com", "h4.com"}, hosts(res.Events), "the newest events, oldest first")
	require.True(t, res.Truncated)
	require.Empty(t, res.NextCursor)
	require.NotEmpty(t, res.Events[0].ID)
}

func TestReadMovesFromGatewayToStoreWhenStoreGetsData(t *testing.T) {
	since := now.Add(-time.Hour)
	s := openStore(t)
	gw, _ := fakeGateway(gatewayLog(2, since.Add(time.Minute)), nil)
	q := query(s, gw, since)

	first, err := audit.Read(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, audit.SourceGateway, first.Coverage.Source)
	require.True(t, first.Coverage.StoreEmpty)
	require.Len(t, first.Events, 2)
	require.Empty(t, first.NextCursor)

	add(t, s, decisionEvent("u1", sandboxName, "id1", since.Add(time.Minute), "a.com"))
	q.After = first.NextCursor
	second, err := audit.Read(t.Context(), q)

	require.NoError(t, err)
	require.Equal(t, audit.SourceStore, second.Coverage.Source)
	require.Equal(t, []string{"u1"}, ids(second.Events))
	require.NotEmpty(t, second.NextCursor)
}

func TestGatewayIDsAreDeterministicAndDistinguishDuplicates(t *testing.T) {
	since := now.Add(-time.Hour)
	at := since.Add(time.Minute)
	same := openshell.Decision{Time: at, Allowed: true, Kind: "HTTP", Host: "dup.com"}
	other := openshell.Decision{Time: at, Allowed: false, Kind: "HTTP", Host: "dup.com"}
	gw := func(context.Context, string, time.Time) ([]openshell.Decision, error) {
		return []openshell.Decision{same, other, same}, nil
	}

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
