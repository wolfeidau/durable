package tracing

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ParseHeaders reads the OTEL_EXPORTER_OTLP_HEADERS format. Errors give only the entry's position because the content is a secret.
func ParseHeaders(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("no headers")
	}
	headers := map[string]string{}
	for i, entry := range strings.Split(s, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("entry %d is not key=value", i+1)
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("entry %d has a bad escape", i+1)
		}
		for have := range headers {
			if strings.EqualFold(have, key) {
				return nil, fmt.Errorf("entry %d repeats header %s", i+1, key)
			}
		}
		headers[key] = decoded
	}
	if len(headers) == 0 {
		return nil, errors.New("no headers")
	}
	return headers, nil
}
