package openshell

import (
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// want matches a Decision; empty fields match anything. Path and Binary are globs: "*" is within one segment and "**" spans segments.
type want struct {
	Allowed                                  bool
	Kind, Binary, Method, Host, Path, Policy string
}

func (w want) matches(d Decision) bool {
	return w.Allowed == d.Allowed &&
		(w.Kind == "" || w.Kind == d.Kind) &&
		(w.Method == "" || w.Method == d.Method) &&
		(w.Host == "" || w.Host == d.Host) &&
		// Grant policies are logged with a prefix; tests name the grant.
		(w.Policy == "" || w.Policy == d.Policy || grantRulePrefix+w.Policy == d.Policy) &&
		(w.Binary == "" || globMatch(w.Binary, d.Binary)) &&
		(w.Path == "" || globMatch(w.Path, d.Path))
}

func globMatch(pattern, s string) bool {
	return globSegments(strings.Split(pattern, "/"), strings.Split(s, "/"))
}

func globSegments(p, s []string) bool {
	if len(p) == 0 {
		return len(s) == 0
	}
	if p[0] == "**" {
		for i := 0; i <= len(s); i++ {
			if globSegments(p[1:], s[i:]) {
				return true
			}
		}
		return false
	}
	if len(s) == 0 {
		return false
	}
	ok, err := path.Match(p[0], s[0])
	return err == nil && ok && globSegments(p[1:], s[1:])
}

// requireDecisions waits for every want to match a decision logged since the given time, because the supervisor flushes logs asynchronously.
func requireDecisions(t *testing.T, l *Launcher, name string, since time.Time, wants ...want) {
	t.Helper()
	var ds []Decision
	missing := func() []want {
		var m []want
		for _, w := range wants {
			if !slices.ContainsFunc(ds, w.matches) {
				m = append(m, w)
			}
		}
		return m
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var err error
		ds, err = l.Decisions(t.Context(), name, since)
		require.NoError(t, err)
		m := missing()
		if len(m) == 0 {
			return
		}
		if time.Now().After(deadline) {
			var buf strings.Builder
			require.NoError(t, FormatDecisions(&buf, ds))
			t.Fatalf("no decision matched %+v\n%s", m, buf.String())
		}
		time.Sleep(time.Second)
	}
}

// requireNoDecision fails when a decision logged since the given time matches.
func requireNoDecision(t *testing.T, l *Launcher, name string, since time.Time, w want) {
	t.Helper()
	ds, err := l.Decisions(t.Context(), name, since)
	require.NoError(t, err)
	if i := slices.IndexFunc(ds, w.matches); i >= 0 {
		var buf strings.Builder
		require.NoError(t, FormatDecisions(&buf, ds))
		t.Fatalf("unexpected decision %+v\n%s", ds[i], buf.String())
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"/data/*", "/data/jq.toml", true},
		{"/data/*", "/data/a/b", false},
		{"/*/*/releases/download/**", "/o/r/releases/download/v1/x.tgz", true},
		{"/*/*/releases/download/**", "/o/r/releases/download", true},
		{"/*/*/releases/download/**", "/o/r/releases", false},
		{"**/curl", "/usr/bin/curl", true},
		{"**/curl", "/usr/bin/curl2", false},
		{"/go/**", "/go/go1.27.linux-arm64.tar.gz", true},
	}
	for _, tc := range tests {
		require.Equal(t, tc.want, globMatch(tc.pattern, tc.s), "%s vs %s", tc.pattern, tc.s)
	}
}
