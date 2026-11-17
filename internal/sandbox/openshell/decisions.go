package openshell

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
)

// decisionLogLines is a ceiling, because the gateway's own log buffer is smaller.
const decisionLogLines = 5000

// Decision is one network request the sandbox proxy allowed or denied.
type Decision struct {
	Time    time.Time
	Allowed bool
	Kind    string // "NET" or "HTTP"
	Binary  string // NET only, without the "(pid)" suffix
	Method  string // HTTP only
	Host    string // host:port for NET, host for HTTP
	Path    string // HTTP only, no query
	Policy  string // empty when none
	Reason  string
}

// parseDecision reads one sandbox log line; it reports false for anything that isn't a network decision.
func parseDecision(line v1.LogLine) (Decision, bool) {
	if line.Target != "ocsf" {
		return Decision{}, false
	}
	head, rest, ok := strings.Cut(line.Message, " ")
	if !ok {
		return Decision{}, false
	}
	kind, op, ok := strings.Cut(head, ":")
	if !ok || (kind != "NET" || op != "OPEN") && kind != "HTTP" {
		return Decision{}, false
	}
	// Skip the "[INFO]" severity.
	_, rest, ok = strings.Cut(rest, "] ")
	if !ok {
		return Decision{}, false
	}
	verdict, rest, _ := strings.Cut(rest, " ")
	if verdict != "ALLOWED" && verdict != "DENIED" {
		return Decision{}, false
	}
	// No token in the body holds a space, so the first " [" starts the attributes.
	body, attrs, _ := strings.Cut(rest, " [")
	d := Decision{Time: line.Timestamp, Allowed: verdict == "ALLOWED", Kind: kind}
	if kind == "NET" {
		bin, host, ok := strings.Cut(body, " -> ")
		if !ok {
			return Decision{}, false
		}
		if i := strings.LastIndex(bin, "("); i >= 0 {
			bin = bin[:i]
		}
		d.Binary, d.Host = bin, host
	} else {
		method, raw, ok := strings.Cut(body, " ")
		if !ok {
			return Decision{}, false
		}
		u, err := url.Parse(raw)
		if err != nil {
			return Decision{}, false
		}
		d.Method, d.Host, d.Path = method, u.Hostname(), u.Path
	}
	if _, v, ok := strings.Cut(attrs, "policy:"); ok {
		d.Policy = v[:strings.IndexAny(v+" ", " ]")]
	}
	if _, v, ok := strings.Cut(attrs, "reason:"); ok {
		d.Reason = strings.TrimSuffix(v, "]")
	}
	return d, true
}

// Decisions returns the network decisions the sandbox logged since the given time.
func (l *Launcher) Decisions(ctx context.Context, name string, since time.Time) ([]Decision, error) {
	res, err := l.c.Sandboxes().GetLogs(ctx, l.ws, name, v1.WithLogSources("sandbox"), v1.WithLogLines(decisionLogLines), v1.WithLogSince(since))
	if err != nil {
		return nil, fmt.Errorf("failed to read sandbox logs: %w", err)
	}
	var ds []Decision
	for _, line := range res.Lines {
		if d, ok := parseDecision(line); ok {
			ds = append(ds, d)
		}
	}
	return ds, nil
}

// Pattern is the path with versioned, numeric and hash-like segments shown as "*", so repeated requests group.
func (d Decision) Pattern() string {
	if d.Path == "" {
		return ""
	}
	segs := strings.Split(d.Path, "/")
	for i, s := range segs {
		if versionLike(s) {
			segs[i] = "*"
		}
	}
	return strings.Join(segs, "/")
}

func versionLike(s string) bool {
	if len(s) >= 32 || versionRE.MatchString(s) {
		return true
	}
	s = strings.TrimPrefix(s, "v")
	return s != "" && s[0] >= '0' && s[0] <= '9'
}

var versionRE = regexp.MustCompile(`\d+\.\d+`)

// FormatDecisions writes one row per distinct request shape, with how often it happened.
func FormatDecisions(w io.Writer, ds []Decision) error {
	type key struct {
		allowed                                  bool
		kind, binary, method, host, path, policy string
	}
	counts := map[key]int{}
	for _, d := range ds {
		counts[key{d.Allowed, d.Kind, d.Binary, d.Method, d.Host, d.Pattern(), d.Policy}]++
	}
	keys := make([]key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.host, b.host), cmp.Compare(a.path, b.path), cmp.Compare(a.binary, b.binary),
			cmp.Compare(a.method, b.method), cmp.Compare(a.policy, b.policy), cmp.Compare(a.kind, b.kind), cmpBool(a.allowed, b.allowed))
	})
	dash := func(s string) string { return cmp.Or(s, "-") }
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "DECISION\tKIND\tBINARY\tMETHOD\tHOST\tPATH\tPOLICY\tCOUNT")
	for _, k := range keys {
		verdict := "denied"
		if k.allowed {
			verdict = "allowed"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", verdict, k.kind, dash(k.binary), dash(k.method), k.host, dash(k.path), dash(k.policy), counts[k])
	}
	return tw.Flush()
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}
