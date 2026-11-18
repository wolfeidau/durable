package telemetry

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func uids(rows []StoredDecision) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.UID
	}
	return out
}

func seedPaging(t *testing.T) (*Store, time.Time) {
	t.Helper()
	s, _ := openStore(t)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// c, a and b share a millisecond, so uid breaks the tie.
	mustAdd(t, s, []Event{
		ev("c", "a", "id1", t0, true, "1.com"),
		ev("a", "a", "id1", t0, true, "2.com"),
		ev("b", "a", "id1", t0, true, "3.com"),
		ev("d", "a", "id1", t0.Add(time.Second), true, "4.com"),
		ev("e", "a", "id1", t0.Add(2*time.Second), true, "5.com"),
		ev("other", "b", "id2", t0, true, "6.com"),
	})
	return s, t0
}

func TestDecisionsPageFirstLoadOrdersByTimeThenUID(t *testing.T) {
	s, t0 := seedPaging(t)

	p, err := s.DecisionsPage(t.Context(), "a", t0, 0, 0)

	require.NoError(t, err)
	require.False(t, p.More)
	require.False(t, p.Reset)
	require.Equal(t, []string{"a", "b", "c", "d", "e"}, uids(p.Rows))
	require.Equal(t, "2.com", p.Rows[0].Host)
}

func TestDecisionsPageFirstLoadIsNewestN(t *testing.T) {
	s, t0 := seedPaging(t)

	p, err := s.DecisionsPage(t.Context(), "a", t0, 0, 3)
	require.NoError(t, err)
	require.True(t, p.More, "older rows in the window were left out")
	require.Equal(t, []string{"c", "d", "e"}, uids(p.Rows), "oldest first")

	p, err = s.DecisionsPage(t.Context(), "a", t0, 0, 5)
	require.NoError(t, err)
	require.False(t, p.More, "limit equal to the row count leaves nothing out")
	require.Len(t, p.Rows, 5)

	p, err = s.DecisionsPage(t.Context(), "a", t0.Add(time.Second), 0, 1)
	require.NoError(t, err)
	require.True(t, p.More)
	require.Equal(t, []string{"e"}, uids(p.Rows))
}

func TestDecisionsPageFirstLoadCursorIsTheNewestInsertion(t *testing.T) {
	s, t0 := seedPaging(t)

	// The newest-by-time rows are not the newest insertions, so the cursor must not follow the page's last row.
	mustAdd(t, s, []Event{ev("late", "a", "id1", t0.Add(-time.Hour), true, "late.com")})
	first, err := s.DecisionsPage(t.Context(), "a", t0.Add(-2*time.Hour), 0, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"d", "e"}, uids(first.Rows))
	require.True(t, first.More)

	next, err := s.DecisionsPage(t.Context(), "a", t0.Add(-2*time.Hour), first.Next, 10)
	require.NoError(t, err)
	require.Empty(t, next.Rows, "nothing is repeated or missed after the first load")
	require.Equal(t, first.Next, next.Next)
	require.False(t, next.More)
}

func TestDecisionsPageResumeReturnsLateArrivals(t *testing.T) {
	s, t0 := seedPaging(t)
	first, err := s.DecisionsPage(t.Context(), "a", t0, 0, 10)
	require.NoError(t, err)

	// A retried export lands rows older than the newest row already returned.
	mustAdd(t, s, []Event{
		ev("late1", "a", "id1", t0.Add(-time.Minute), true, "l1.com"),
		ev("new", "a", "id1", t0.Add(time.Hour), true, "n.com"),
		ev("elsewhere", "b", "id2", t0.Add(time.Hour), true, "x.com"),
	})
	second, err := s.DecisionsPage(t.Context(), "a", t0.Add(-time.Hour), first.Next, 10)

	require.NoError(t, err)
	require.Equal(t, []string{"late1", "new"}, uids(second.Rows), "insertion order, including the older time")
	require.Greater(t, second.Next, first.Next)
	require.False(t, second.More)
	require.False(t, second.Reset)
}

func TestDecisionsPageResumeRespectsSinceAndLimit(t *testing.T) {
	s, t0 := seedPaging(t)
	first, err := s.DecisionsPage(t.Context(), "a", t0, 0, 10)
	require.NoError(t, err)
	mustAdd(t, s, []Event{
		ev("x1", "a", "id1", t0.Add(time.Minute), true, "1.com"),
		ev("old", "a", "id1", t0.Add(-time.Hour), true, "2.com"),
		ev("x2", "a", "id1", t0.Add(2*time.Minute), true, "3.com"),
		ev("x3", "a", "id1", t0.Add(3*time.Minute), true, "4.com"),
	})

	var seen []string
	after := first.Next
	for range 5 {
		p, err := s.DecisionsPage(t.Context(), "a", t0, after, 2)
		require.NoError(t, err)
		seen = append(seen, uids(p.Rows)...)
		after = p.Next
		if !p.More {
			break
		}
	}

	require.Equal(t, []string{"x1", "x2", "x3"}, seen, "the row before since is skipped; paging walks each row once")
}

func TestDecisionsPageCursorBeyondTheStoreResets(t *testing.T) {
	s, t0 := seedPaging(t)
	first, err := s.DecisionsPage(t.Context(), "a", t0, 0, 10)
	require.NoError(t, err)

	// Pruning every row lets SQLite hand out the same row ids again.
	_, err = s.Prune(t.Context(), t0.Add(24*time.Hour))
	require.NoError(t, err)
	mustAdd(t, s, []Event{ev("fresh", "a", "id1", t0.Add(25*time.Hour), true, "f.com")})
	p, err := s.DecisionsPage(t.Context(), "a", t0, first.Next+100, 10)

	require.NoError(t, err)
	require.True(t, p.Reset)
	require.Equal(t, []string{"fresh"}, uids(p.Rows), "a reset is a first load")
	require.Positive(t, p.Next)
}

func TestDecisionsPageLimitBounds(t *testing.T) {
	s, t0 := seedPaging(t)

	for _, limit := range []int{0, -1} {
		p, err := s.DecisionsPage(t.Context(), "a", t0, 0, limit)
		require.NoError(t, err)
		require.False(t, p.More, fmt.Sprint(limit))
		require.Len(t, p.Rows, 5)
	}

	p, err := s.DecisionsPage(t.Context(), "nobody", t0, 0, 5)
	require.NoError(t, err)
	require.Empty(t, p.Rows)
	require.Zero(t, p.Next)
}

func TestDecisionsPageResumeUsesTheRowidKey(t *testing.T) {
	s, _ := openStore(t)

	rows, err := s.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT rowid FROM decisions WHERE sandbox_name = ? AND time_ms >= ? AND rowid > ? ORDER BY rowid LIMIT 3`, "a", 0, 1)
	require.NoError(t, err)
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan += detail + "\n"
	}
	require.NoError(t, rows.Err())
	require.NotContains(t, plan, "USE TEMP B-TREE", plan)
	t.Log(plan)
}
