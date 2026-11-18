package audit_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/audit"
	"github.com/wolfeidau/durable/internal/otlp"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
	"github.com/wolfeidau/durable/internal/telemetry"
)

const (
	sandboxName = "dt-test"
	retention   = 30 * 24 * time.Hour
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func openStore(t *testing.T) *telemetry.Store {
	t.Helper()
	s, err := telemetry.Open(t.Context(), filepath.Join(t.TempDir(), "telemetry.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func decisionEvent(uid, name, id string, at time.Time, host string) telemetry.Event {
	return telemetry.Event{
		UID: uid, SandboxName: name, SandboxID: id, Time: at, Observed: at,
		Decision: &openshell.Decision{Time: at, Allowed: true, Kind: "HTTP", Method: "GET", Host: host, Path: "/a"},
	}
}

func gapEvent(id string, observed time.Time, dropped int64, since time.Time) telemetry.Event {
	return telemetry.Event{SandboxID: id, Observed: observed, Raw: []byte(`{}`), Gap: &otlp.Gap{Dropped: dropped, Since: since}}
}

func add(t *testing.T, s *telemetry.Store, evs ...telemetry.Event) {
	t.Helper()
	_, _, err := s.Add(t.Context(), evs, time.Time{})
	require.NoError(t, err)
}

func fakeGateway(ds []openshell.Decision, err error) (audit.Gateway, *int) {
	calls := new(int)
	return func(context.Context, string, time.Time) ([]openshell.Decision, error) {
		*calls++
		return ds, err
	}, calls
}

func query(store *telemetry.Store, gw audit.Gateway, since time.Time) audit.Query {
	return audit.Query{Store: store, Gateway: gw, Sandbox: sandboxName, Since: since, Now: now, Retention: retention}
}

func TestReadFromStoreSkipsGateway(t *testing.T) {
	s := openStore(t)
	since := now.Add(-time.Hour)
	add(t, s, decisionEvent("u1", sandboxName, "id1", since.Add(time.Minute), "a.com"))
	gw, calls := fakeGateway(nil, errors.New("unused"))

	res, err := audit.Read(t.Context(), query(s, gw, since))

	require.NoError(t, err)
	require.Len(t, res.Decisions, 1)
	require.Zero(t, *calls)
	require.Equal(t, audit.SourceStore, res.Coverage.Source)
	require.True(t, res.Coverage.StoreAvailable)
	require.False(t, res.Coverage.StoreEmpty)
	require.NoError(t, res.Coverage.GatewayErr)
	require.Equal(t, retention, res.Coverage.Retention)
	require.False(t, res.Coverage.Pruned)
}

func TestReadFallsBackWhenStoreHasNone(t *testing.T) {
	s := openStore(t)
	since := now.Add(-time.Hour)
	gwDecisions := []openshell.Decision{{Time: since.Add(10 * time.Minute), Host: "g.com"}}
	gw, calls := fakeGateway(gwDecisions, nil)

	res, err := audit.Read(t.Context(), query(s, gw, since))

	require.NoError(t, err)
	require.Equal(t, 1, *calls)
	require.Equal(t, gwDecisions, res.Decisions)
	require.Equal(t, audit.SourceGateway, res.Coverage.Source)
	require.True(t, res.Coverage.StoreEmpty)
	require.Equal(t, gwDecisions[0].Time, res.Coverage.GatewayStart)
}

func TestReadWithoutStoreUsesGateway(t *testing.T) {
	since := now.Add(-time.Hour)
	gw, _ := fakeGateway([]openshell.Decision{{Time: since.Add(-time.Minute), Host: "g.com"}}, nil)

	res, err := audit.Read(t.Context(), query(nil, gw, since))

	require.NoError(t, err)
	require.Equal(t, audit.SourceGateway, res.Coverage.Source)
	require.False(t, res.Coverage.StoreAvailable)
	require.False(t, res.Coverage.StoreEmpty)
	require.True(t, res.Coverage.GatewayStart.IsZero(), "the log reaches back past since")
	require.Zero(t, res.Coverage.Retention)
}

func TestReadGatewayErrorIsCoverageNotFailure(t *testing.T) {
	boom := errors.New("gateway down")
	gw, _ := fakeGateway(nil, boom)
	since := now.Add(-time.Hour)

	for name, store := range map[string]*telemetry.Store{"with store": openStore(t), "without store": nil} {
		t.Run(name, func(t *testing.T) {
			res, err := audit.Read(t.Context(), query(store, gw, since))
			require.NoError(t, err)
			require.Empty(t, res.Decisions)
			require.ErrorIs(t, res.Coverage.GatewayErr, boom)
		})
	}
}

func TestReadWithoutGatewayReader(t *testing.T) {
	res, err := audit.Read(t.Context(), query(nil, nil, now.Add(-time.Hour)))

	require.NoError(t, err)
	require.Error(t, res.Coverage.GatewayErr)
}

func TestReadHistoryStartsLate(t *testing.T) {
	s := openStore(t)
	first := now.Add(-time.Hour)
	add(t, s, decisionEvent("u1", sandboxName, "id1", first, "a.com"))

	res, err := audit.Read(t.Context(), query(s, nil, now.Add(-2*time.Hour)))
	require.NoError(t, err)
	require.Equal(t, first, res.Coverage.HistoryStart)

	res, err = audit.Read(t.Context(), query(s, nil, first.Add(-time.Minute)))
	require.NoError(t, err)
	require.Equal(t, first, res.Coverage.HistoryStart)

	res, err = audit.Read(t.Context(), query(s, nil, first))
	require.NoError(t, err)
	require.True(t, res.Coverage.HistoryStart.IsZero(), "history covers the whole window")
}

func TestReadDropCategories(t *testing.T) {
	s := openStore(t)
	since := now.Add(-time.Hour)
	add(t, s,
		decisionEvent("u1", sandboxName, "id1", since.Add(time.Minute), "a.com"),
		gapEvent("id1", now.Add(-30*time.Minute), 3, now.Add(-40*time.Minute)),
		gapEvent("id1", now.Add(-20*time.Minute), 2, now.Add(-25*time.Minute)),
		gapEvent("", now.Add(-10*time.Minute), 5, now.Add(-15*time.Minute)),
		gapEvent("never-seen", now.Add(-5*time.Minute), 7, now.Add(-6*time.Minute)),
	)

	res, err := audit.Read(t.Context(), query(s, nil, since))

	require.NoError(t, err)
	d := res.Coverage.Drops
	require.Equal(t, audit.Drop{Count: 5, Since: now.Add(-40 * time.Minute)}, d.Own)
	require.Equal(t, audit.Drop{Count: 5, Since: now.Add(-15 * time.Minute)}, d.GatewayWide)
	require.Equal(t, audit.Drop{Count: 7, Since: now.Add(-6 * time.Minute)}, d.Unknown)
	require.Zero(t, d.Unattributed)
}

func TestReadUnattributed(t *testing.T) {
	s := openStore(t)
	since := now.Add(-time.Hour)
	unattributed := decisionEvent("", "", "", since.Add(time.Minute), "x.com")
	unattributed.Raw = []byte(`{}`)
	add(t, s, decisionEvent("u1", sandboxName, "id1", since.Add(time.Minute), "a.com"), unattributed)

	res, err := audit.Read(t.Context(), query(s, nil, since))

	require.NoError(t, err)
	require.Equal(t, int64(1), res.Coverage.Drops.Unattributed)
}

func TestReadPrunedWindow(t *testing.T) {
	s := openStore(t)

	res, err := audit.Read(t.Context(), query(s, nil, now.Add(-retention-time.Hour)))
	require.NoError(t, err)
	require.True(t, res.Coverage.Pruned)

	res, err = audit.Read(t.Context(), query(s, nil, now.Add(-retention+time.Hour)))
	require.NoError(t, err)
	require.False(t, res.Coverage.Pruned)
}

func TestReadEmpty(t *testing.T) {
	s := openStore(t)
	gw, _ := fakeGateway(nil, nil)

	res, err := audit.Read(t.Context(), query(s, gw, now.Add(-time.Hour)))

	require.NoError(t, err)
	require.Empty(t, res.Decisions)
	require.Equal(t, audit.SourceGateway, res.Coverage.Source)
	require.True(t, res.Coverage.StoreEmpty)
	require.NoError(t, res.Coverage.GatewayErr)
}
