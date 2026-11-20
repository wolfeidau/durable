package tracing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseHeaders(t *testing.T) {
	tests := map[string]struct {
		in   string
		want map[string]string
	}{
		"single":        {"a=b", map[string]string{"a": "b"}},
		"multiple":      {"a=b,c=d", map[string]string{"a": "b", "c": "d"}},
		"raw space":     {"authorization=Bearer abc123", map[string]string{"authorization": "Bearer abc123"}},
		"percent space": {"authorization=Bearer%20abc123", map[string]string{"authorization": "Bearer abc123"}},
		"plus kept":     {"a=b+c", map[string]string{"a": "b+c"}},
		"equals":        {"a=b=c==", map[string]string{"a": "b=c=="}},
		"whitespace":    {" a = b ,, c=d ,\n", map[string]string{"a": "b", "c": "d"}},
		"empty value":   {"a=", map[string]string{"a": ""}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseHeaders(tt.in)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseHeadersErrorsNeverIncludeTheContent(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"missing equals": {"a=b,fakesecretvalue", "entry 2 is not key=value"},
		"empty key":      {"=fakesecretvalue", "entry 1 is not key=value"},
		"blank key":      {"a=b, =fakesecretvalue", "entry 2 is not key=value"},
		"bad escape":     {"a=fakesecretvalue%zz", "entry 1 has a bad escape"},
		"repeated name":  {"a=b,A=fakesecretvalue", "entry 2 repeats header A"},
		"empty":          {"", "no headers"},
		"only blanks":    {" , \n", "no headers"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHeaders(tt.in)
			require.EqualError(t, err, tt.want)
			require.NotContains(t, err.Error(), "fakesecretvalue")
		})
	}
}
