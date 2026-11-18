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

func TestDecisionsPageOrdersByTimeThenUID(t *testing.T) {
	s, t0 := seedPaging(t)

	rows, more, err := s.DecisionsPage(t.Context(), "a", t0, nil, 0)

	require.NoError(t, err)
	require.False(t, more)
	require.Equal(t, []string{"a", "b", "c", "d", "e"}, uids(rows))
	require.Equal(t, "2.com", rows[0].Host)
}

func TestDecisionsPageAfterIsStrictlyAfter(t *testing.T) {
	s, t0 := seedPaging(t)

	rows, more, err := s.DecisionsPage(t.Context(), "a", t0, &Position{TimeMS: t0.UnixMilli(), UID: "a"}, 0)
	require.NoError(t, err)
	require.False(t, more)
	require.Equal(t, []string{"b", "c", "d", "e"}, uids(rows))

	rows, _, err = s.DecisionsPage(t.Context(), "a", t0, &Position{TimeMS: t0.UnixMilli(), UID: "c"}, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"d", "e"}, uids(rows))

	rows, _, err = s.DecisionsPage(t.Context(), "a", t0, &Position{TimeMS: t0.Add(2 * time.Second).UnixMilli(), UID: "e"}, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestDecisionsPageWalksEveryRowOnce(t *testing.T) {
	s, t0 := seedPaging(t)

	var seen []string
	// Without a cursor the first page is the newest rows, so start from before the oldest.
	after := &Position{TimeMS: t0.UnixMilli() - 1}
	for range 10 {
		rows, more, err := s.DecisionsPage(t.Context(), "a", t0, after, 2)
		require.NoError(t, err)
		seen = append(seen, uids(rows)...)
		if !more {
			break
		}
		last := rows[len(rows)-1]
		after = &Position{TimeMS: last.Time.UnixMilli(), UID: last.UID}
	}

	require.Equal(t, []string{"a", "b", "c", "d", "e"}, seen)
}

func TestDecisionsPageInitialLoadIsNewestN(t *testing.T) {
	s, t0 := seedPaging(t)

	rows, more, err := s.DecisionsPage(t.Context(), "a", t0, nil, 3)
	require.NoError(t, err)
	require.True(t, more, "older rows in the window were left out")
	require.Equal(t, []string{"c", "d", "e"}, uids(rows), "oldest first")

	rows, more, err = s.DecisionsPage(t.Context(), "a", t0, nil, 5)
	require.NoError(t, err)
	require.False(t, more, "limit equal to the row count leaves nothing out")
	require.Len(t, rows, 5)

	rows, more, err = s.DecisionsPage(t.Context(), "a", t0.Add(time.Second), nil, 1)
	require.NoError(t, err)
	require.True(t, more)
	require.Equal(t, []string{"e"}, uids(rows))
}

func TestDecisionsPageLimitBounds(t *testing.T) {
	s, t0 := seedPaging(t)

	for _, limit := range []int{0, -1} {
		rows, more, err := s.DecisionsPage(t.Context(), "a", t0, nil, limit)
		require.NoError(t, err)
		require.False(t, more, fmt.Sprint(limit))
		require.Len(t, rows, 5)
	}

	rows, more, err := s.DecisionsPage(t.Context(), "a", t0, nil, 1)
	require.NoError(t, err)
	require.True(t, more)
	require.Equal(t, []string{"e"}, uids(rows))

	rows, _, err = s.DecisionsPage(t.Context(), "nobody", t0, nil, 5)
	require.NoError(t, err)
	require.Empty(t, rows)
}
