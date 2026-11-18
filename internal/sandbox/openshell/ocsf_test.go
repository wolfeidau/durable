package openshell

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func withTime(d Decision, t time.Time) Decision {
	d.Time = t
	return d
}

func TestDecisionFromOCSF(t *testing.T) {
	at := time.UnixMilli(1700000000123).UTC()
	tests := []struct {
		name string
		body string
		want Decision
		ok   bool
	}{
		{
			name: "allowed NET",
			body: `{"class_uid":4001,"activity_id":1,"action_id":1,"time":1700000000123,"message":"CONNECT api.example.com:443 allowed",
				"dst_endpoint":{"domain":"api.example.com","port":443},"actor":{"process":{"name":"/usr/bin/python3","pid":42}},
				"firewall_rule":{"name":"default-egress","type":"mechanistic"}}`,
			want: Decision{Time: at, Allowed: true, Kind: "NET", Activity: "OPEN", Binary: "/usr/bin/python3", Host: "api.example.com:443", Policy: "default-egress"},
			ok:   true,
		},
		{
			name: "denied NET with no policy",
			body: `{"class_uid":4001,"activity_id":1,"action_id":2,"time":1700000000123,"status_detail":"endpoint example.com:443 not in policy",
				"dst_endpoint":{"ip":"93.184.216.34","port":443},"actor":{"process":{"name":"/usr/bin/curl","pid":7}}}`,
			want: Decision{Time: at, Kind: "NET", Activity: "OPEN", Binary: "/usr/bin/curl", Host: "93.184.216.34:443", Reason: "endpoint example.com:443 not in policy"},
			ok:   true,
		},
		{
			name: "allowed HTTP strips the query",
			body: `{"class_uid":4002,"activity_id":3,"action_id":1,"time":1700000000123,
				"http_request":{"http_method":"GET","url":{"scheme":"https","hostname":"github.com","path":"/o/r/releases/download/v1/x.tgz?token=1","port":443}},
				"firewall_rule":{"name":"mise","type":"l7"}}`,
			want: Decision{Time: at, Allowed: true, Kind: "HTTP", Method: "GET", Host: "github.com", Path: "/o/r/releases/download/v1/x.tgz", Policy: "mise"},
			ok:   true,
		},
		{
			name: "denied HTTP with reason",
			body: `{"class_uid":4002,"activity_id":3,"action_id":2,"time":1700000000123,"message":"FORWARD blocked","status_detail":"resolves to always-blocked address",
				"http_request":{"http_method":"PUT","url":{"scheme":"http","hostname":"169.254.169.254","path":"/latest/api/token","port":80}},
				"firewall_rule":{"name":"aws_iam","type":"ssrf"}}`,
			want: Decision{Time: at, Kind: "HTTP", Method: "PUT", Host: "169.254.169.254", Path: "/latest/api/token", Policy: "aws_iam", Reason: "resolves to always-blocked address"},
			ok:   true,
		},
		{
			name: "bypass refusal",
			body: `{"class_uid":4001,"activity_id":5,"action_id":2,"time":1700000000123,"status_detail":"bypass",
				"dst_endpoint":{"ip":"93.184.216.34","port":443},"actor":{"process":{"name":"node","pid":1234}},
				"connection_info":{"protocol_name":"tcp"},"firewall_rule":{"name":"bypass-detect","type":"nftables"}}`,
			want: withTime(bypassRefusal, at), ok: true,
		},
		{
			name: "DNS refusal without port or actor",
			body: `{"class_uid":4001,"activity_id":5,"action_id":2,"time":1700000000123,"message":"Policy DNS refused a name that is not eligible in the active policy",
				"status_detail":"policy_dns_ineligible","dst_endpoint":{"domain":"mise-versions.jdx.dev"}}`,
			want: withTime(dnsRefusal, at), ok: true,
		},
		{name: "refusal without destination", body: `{"class_uid":4001,"activity_id":5,"action_id":2}`},
		{name: "network activity other than open or refuse", body: `{"class_uid":4001,"activity_id":6,"action_id":2,"dst_endpoint":{"ip":"1.2.3.4","port":1}}`},
		{name: "non-network class", body: `{"class_uid":6002,"activity_id":1,"action_id":1}`},
		{name: "no action", body: `{"class_uid":4001,"activity_id":1,"dst_endpoint":{"domain":"a.com","port":1}}`},
		{name: "malformed", body: `{"class_uid":`},
		{name: "empty", body: ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decisionFromOCSF([]byte(tc.body))
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
	_, ok := decisionFromOCSF(nil)
	require.False(t, ok)
}

func TestDecisionFromOCSFFixtures(t *testing.T) {
	tests := []struct {
		file string
		want Decision
		ok   bool
	}{
		{"net-allowed-grant.json", Decision{Allowed: true, Kind: "NET", Activity: "OPEN", Binary: "/usr/local/bin/mise", Host: "release-assets.githubusercontent.com:443", Policy: "durable-grant-mise"}, true},
		{"net-denied-curl.json", Decision{Kind: "NET", Activity: "OPEN", Binary: "/usr/bin/curl", Host: "github.com:443", Reason: "transparent_tcp_policy_denied"}, true},
		{"net-refused-dns.json", Decision{Kind: "NET", Activity: "REFUSE", Host: "mise-versions.jdx.dev", Reason: "policy_dns_ineligible"}, true},
		{"http-allowed-grant.json", Decision{Allowed: true, Kind: "HTTP", Method: "GET", Host: "github.com", Path: "/jqlang/jq/releases/download/jq-1.8.2/jq-linux-arm64", Policy: "durable-grant-mise"}, true},
		{"net-session-established.json", Decision{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "ocsf", tc.file))
			require.NoError(t, err)
			got, ok := decisionFromOCSF(body)
			require.Equal(t, tc.ok, ok)
			var e ocsfEvent
			require.NoError(t, json.Unmarshal(body, &e))
			tc.want.Time = time.UnixMilli(e.Time).UTC()
			if !tc.ok {
				tc.want.Time = time.Time{}
			}
			require.Equal(t, tc.want, got)
		})
	}
}
