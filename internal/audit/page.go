package audit

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/wolfeidau/durable/internal/sandbox/openshell"
)

// gatewayEvents gives each log decision an id derived from its content, ordered by (time, id).
// Identical decisions in the same millisecond get a counter, in log order. The ids let a client dedupe repeated reads;
// they are not stable once the log drops lines, so there is no cursor.
func gatewayEvents(ds []openshell.Decision) []Event {
	seen := map[string]int{}
	out := make([]Event, len(ds))
	for i, d := range ds {
		sum := sha256.Sum256(fmt.Appendf(nil, "%d\x00%t\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
			d.Time.UnixMilli(), d.Allowed, d.Kind, d.Activity, d.Binary, d.Method, d.Host, d.Path, d.Policy, d.Reason))
		key := hex.EncodeToString(sum[:16])
		n := seen[key]
		seen[key]++
		out[i] = Event{ID: "g-" + key + "-" + strconv.Itoa(n), Decision: d}
	}
	slices.SortStableFunc(out, func(a, b Event) int {
		if c := cmp.Compare(a.Time.UnixMilli(), b.Time.UnixMilli()); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}
