package audit

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/wolfeidau/durable/internal/sandbox/openshell"
)

// cursorTag marks a telemetry store cursor, so a cursor from any other source is rejected.
const cursorTag = "s:"

func encodeCursor(rowID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorTag + strconv.FormatInt(rowID, 10)))
}

// parseCursor returns zero for an empty cursor.
func parseCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	rest, ok := strings.CutPrefix(string(raw), cursorTag)
	if err != nil || !ok {
		return 0, fmt.Errorf("%w: not a cursor from this API", ErrBadCursor)
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: not a cursor from this API", ErrBadCursor)
	}
	return id, nil
}

// gatewayEvents gives each log decision an id derived from its content, ordered by (time, id).
// Identical decisions in the same millisecond get a counter, in log order. The ids let a client dedupe repeated reads;
// they are not stable once the log drops lines, which is why the log source issues no cursor.
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
