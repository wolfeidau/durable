package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/otlp"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func ev(uid, name, id string, at time.Time, allowed bool, host string) Event {
	return Event{
		UID: uid, SandboxName: name, SandboxID: id, Observed: at,
		Decision: openshell.Decision{Time: at, Allowed: allowed, Kind: "HTTP", Method: "GET", Host: host, Path: "/a/v1.2.3/b", Policy: "p"},
	}
}

func mustAdd(t *testing.T, s *Store, evs []Event) int {
	t.Helper()
	skipped, err := s.Add(context.Background(), evs)
	require.NoError(t, err)
	return skipped
}

func TestAddAndDecisions(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	full := Event{
		UID: "u2", SandboxName: "a", SandboxID: "id1", Observed: t0.Add(time.Minute),
		Decision: openshell.Decision{Time: t0.Add(time.Second), Kind: "NET", Activity: "REFUSE", Binary: "/bin/curl", Host: "h:443", Reason: "nope"},
	}
	mustAdd(t, s, []Event{
		full,
		ev("u1", "a", "id1", t0, true, "github.com"),
		ev("u3", "b", "id2", t0, true, "other.com"),
		ev("u4", "a", "id1", t0.Add(-time.Hour), true, "old.com"),
	})

	got, err := s.Decisions(ctx, "a", t0)
	require.NoError(t, err)
	require.Equal(t, []openshell.Decision{
		{Time: t0, Allowed: true, Kind: "HTTP", Method: "GET", Host: "github.com", Path: "/a/v1.2.3/b", Policy: "p"},
		full.Decision,
	}, got)
	require.Equal(t, time.UTC, got[0].Time.Location())

	all, err := s.Decisions(ctx, "a", time.Time{})
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "old.com", all[0].Host)
}

func TestDuplicatesCountOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e := ev("u1", "a", "id1", t0, false, "x.com")
	mustAdd(t, s, []Event{e, e})
	mustAdd(t, s, []Event{e})

	ds, err := s.Decisions(ctx, "a", time.Time{})
	require.NoError(t, err)
	require.Len(t, ds, 1)
	days, err := s.Daily(ctx, "a", t0)
	require.NoError(t, err)
	require.Equal(t, []Day{{Day: "2026-01-02", Denied: 1, TopDenied: []HostCount{{"x.com", 1}}}}, days)
}

func TestDailyAcrossDayBoundary(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	d1 := time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC)
	d2 := time.Date(2026, 1, 3, 0, 1, 0, 0, time.UTC)
	evs := []Event{
		ev("1", "a", "i", d1, true, "ok.com"),
		ev("2", "a", "i", d1, false, "d1.com"),
		ev("3", "a", "i", d2, true, "ok.com"),
		ev("4", "a", "i", d2, true, "ok.com"),
		ev("5", "b", "i", d2, false, "elsewhere.com"),
	}
	// d2 denied: z x3 (a DNS refusal has no port), b x2, a x2, c x1, so the top three are z, a, b.
	n := 10
	for _, h := range []struct {
		host string
		cnt  int
	}{{"z.com:443", 2}, {"z.com", 1}, {"b.com", 2}, {"a.com", 2}, {"c.com", 1}} {
		for range h.cnt {
			n++
			evs = append(evs, ev(fmt.Sprint(n), "a", "i", d2, false, h.host))
		}
	}
	mustAdd(t, s, evs)

	days, err := s.Daily(ctx, "a", d1)
	require.NoError(t, err)
	require.Equal(t, []Day{
		{Day: "2026-01-02", Allowed: 1, Denied: 1, TopDenied: []HostCount{{"d1.com", 1}}},
		{Day: "2026-01-03", Allowed: 2, Denied: 8, TopDenied: []HostCount{{"z.com", 3}, {"a.com", 2}, {"b.com", 2}}},
	}, days)

	// from is truncated to its UTC day.
	days, err = s.Daily(ctx, "a", d2.Add(5*time.Hour))
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, "2026-01-03", days[0].Day)
}

func TestPruneKeepsDaily(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cut := old.Add(24 * time.Hour)
	mustAdd(t, s, []Event{
		ev("1", "a", "i", old, true, "a.com"),
		ev("2", "a", "i", cut, true, "a.com"),
		{SandboxID: "i", Observed: old, Gap: &otlp.Gap{Dropped: 3, Since: old}},
		{SandboxID: "i", Observed: cut, Gap: &otlp.Gap{Dropped: 4, Since: cut}},
	})

	n, err := s.Prune(ctx, cut)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)

	ds, err := s.Decisions(ctx, "a", time.Time{})
	require.NoError(t, err)
	require.Len(t, ds, 1)
	gs, err := s.Gaps(ctx, []string{"i"}, time.Time{})
	require.NoError(t, err)
	require.Len(t, gs, 1)
	days, err := s.Daily(ctx, "a", old)
	require.NoError(t, err)
	require.Len(t, days, 2)
}

func TestGaps(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	mustAdd(t, s, []Event{
		{SandboxID: "i1", Observed: t0, Gap: &otlp.Gap{Dropped: 5, Since: t0.Add(-time.Minute)}},
		{SandboxID: "", Observed: t0.Add(time.Minute), Gap: &otlp.Gap{Dropped: 7, Since: t0}},
		{SandboxID: "other", Observed: t0, Gap: &otlp.Gap{Dropped: 9, Since: t0}},
		{SandboxID: "i1", Observed: t0.Add(-time.Hour), Gap: &otlp.Gap{Dropped: 1, Since: t0.Add(-2 * time.Hour)}},
	})

	got, err := s.Gaps(ctx, []string{"i1"}, t0)
	require.NoError(t, err)
	require.Equal(t, []GapInfo{
		{SandboxID: "i1", Observed: t0, Since: t0.Add(-time.Minute), Dropped: 5},
		{SandboxID: "", Observed: t0.Add(time.Minute), Since: t0, Dropped: 7},
	}, got)

	got, err = s.Gaps(ctx, nil, t0.Add(-2*time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Empty(t, got[0].SandboxID)
}

func TestSandboxIDs(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	mustAdd(t, s, []Event{
		ev("1", "a", "id-old", t0, true, "x"),
		ev("2", "a", "id-new", t0, true, "x"),
		ev("3", "a", "id-new", t0, true, "y"),
		ev("4", "b", "id-b", t0, true, "x"),
	})
	ids, err := s.SandboxIDs(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, []string{"id-new", "id-old"}, ids)
}

func TestAddCountsUnattributablePerDay(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	d1 := time.Date(2026, 1, 2, 23, 0, 0, 0, time.UTC)
	d2 := d1.Add(2 * time.Hour)
	require.Equal(t, 3, mustAdd(t, s, []Event{
		ev("", "a", "i", d1, true, "x"),
		ev("1", "", "i", d1, true, "x"),
		ev("", "", "i", d2, true, "x"),
		ev("2", "a", "i", d1, true, "x"),
	}))
	require.Zero(t, mustAdd(t, s, nil))
	require.Zero(t, mustAdd(t, s, []Event{}))

	var n int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM decisions) + (SELECT COUNT(*) FROM daily) + (SELECT COUNT(*) FROM gaps)`).Scan(&n))
	require.Equal(t, 2, n)

	got, err := s.Unattributed(ctx, d1)
	require.NoError(t, err)
	require.EqualValues(t, 3, got)
	got, err = s.Unattributed(ctx, d2)
	require.NoError(t, err)
	require.EqualValues(t, 1, got)
	got, err = s.Unattributed(ctx, d2.Add(48*time.Hour))
	require.NoError(t, err)
	require.Zero(t, got)
}

func TestEarliestDecisionAndFirstDay(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	_, ok, err := s.EarliestDecision(ctx, "a")
	require.NoError(t, err)
	require.False(t, ok)
	_, ok, err = s.FirstDay(ctx, "a")
	require.NoError(t, err)
	require.False(t, ok)

	mustAdd(t, s, []Event{ev("1", "a", "i", t0.Add(time.Hour), true, "x"), ev("2", "a", "i", t0, true, "x"), ev("3", "b", "i", t0.Add(-48*time.Hour), true, "x")})
	got, ok, err := s.EarliestDecision(ctx, "a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, t0, got)

	_, err = s.Prune(ctx, t0.Add(30*time.Minute))
	require.NoError(t, err)
	got, _, err = s.EarliestDecision(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, t0.Add(time.Hour), got)
	dy, ok, err := s.FirstDay(ctx, "a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "2026-01-02", dy)
}

func TestRawEventRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e := ev("u1", "a", "i", t0, true, "x")
	e.Body = []byte(`{"cmd":"curl x"}`)
	old := ev("u0", "a", "i", t0.Add(-time.Hour), true, "x")
	old.Body = []byte(`{"old":1}`)
	other := ev("u2", "b", "i", t0, true, "x")
	other.Body = []byte(`{"b":1}`)
	mustAdd(t, s, []Event{e, old, other})

	got, err := s.RawEvents(ctx, "a", t0)
	require.NoError(t, err)
	require.Equal(t, []RawEvent{{UID: "u1", Time: t0, Body: json.RawMessage(`{"cmd":"curl x"}`)}}, got)
	got, err = s.RawEvents(ctx, "a", time.Time{})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "u0", got[0].UID)
}

func TestRawEventDuplicateStoresOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	e := ev("u1", "a", "i", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), true, "x")
	e.Body = []byte(`{"n":1}`)
	dup := e
	dup.Body = []byte(`{"n":2}`)
	mustAdd(t, s, []Event{e, dup})
	mustAdd(t, s, []Event{dup})

	got, err := s.RawEvents(ctx, "a", time.Time{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.JSONEq(t, `{"n":1}`, string(got[0].Body))
}

func TestRawEventSkippedWithoutBodyOrSandbox(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	noBody := ev("1", "a", "i", t0, true, "x")
	noSandbox := ev("2", "", "i", t0, true, "x")
	noSandbox.Body = []byte(`{"n":1}`)
	mustAdd(t, s, []Event{noBody, noSandbox})

	var n int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_events`).Scan(&n))
	require.Zero(t, n)
}

func TestPruneRemovesRawEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cut := old.Add(24 * time.Hour)
	a, b := ev("1", "a", "i", old, true, "x"), ev("2", "a", "i", cut, true, "x")
	a.Body, b.Body = []byte(`{"n":1}`), []byte(`{"n":2}`)
	mustAdd(t, s, []Event{a, b})

	n, err := s.Prune(ctx, cut)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	got, err := s.RawEvents(ctx, "a", time.Time{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "2", got[0].UID)
}

func TestZeroDecisionTimeUsesObserved(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	obs := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	e := ev("1", "a", "i", obs, true, "x")
	e.Decision.Time = time.Time{}
	mustAdd(t, s, []Event{e})
	days, err := s.Daily(ctx, "a", obs)
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, 1, days[0].Allowed)
}

func TestOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	s, path := openStore(t)
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	mustAdd(t, s, []Event{ev("1", "a", "i", t0, true, "x")})

	ro, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	ds, err := ro.Decisions(ctx, "a", time.Time{})
	require.NoError(t, err)
	require.Len(t, ds, 1)
	_, err = ro.Add(ctx, []Event{ev("2", "a", "i", t0, true, "x")})
	require.Error(t, err)

	missing := filepath.Join(t.TempDir(), "missing.db")
	_, err = OpenReadOnly(ctx, missing)
	require.Error(t, err)
	_, statErr := os.Stat(missing)
	require.True(t, os.IsNotExist(statErr))
}

func TestFixtures(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	files, err := filepath.Glob(filepath.Join("..", "sandbox", "openshell", "testdata", "ocsf", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	observed := time.Now().UTC()
	var evs []Event
	for _, f := range files {
		body, err := os.ReadFile(f)
		require.NoError(t, err)
		d, name, id, ok := openshell.ParseOCSF(body)
		if !ok {
			continue
		}
		evs = append(evs, Event{Decision: d, UID: filepath.Base(f), SandboxName: name, SandboxID: id, Observed: observed})
	}
	require.NotEmpty(t, evs)
	mustAdd(t, s, evs)

	ds, err := s.Decisions(ctx, "dt-63539bd5bc24", time.Time{})
	require.NoError(t, err)
	require.Len(t, ds, len(evs))
	ids, err := s.SandboxIDs(ctx, "dt-63539bd5bc24")
	require.NoError(t, err)
	require.Equal(t, []string{"126316d7-ba94-4724-b97b-50ba61698af0"}, ids)
}
