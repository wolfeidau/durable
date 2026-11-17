package openshell

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
			want: Decision{Time: at, Allowed: true, Kind: "NET", Binary: "/usr/bin/python3", Host: "api.example.com:443", Policy: "default-egress"},
			ok:   true,
		},
		{
			name: "denied NET with no policy",
			body: `{"class_uid":4001,"activity_id":1,"action_id":2,"time":1700000000123,"status_detail":"endpoint example.com:443 not in policy",
				"dst_endpoint":{"ip":"93.184.216.34","port":443},"actor":{"process":{"name":"/usr/bin/curl","pid":7}}}`,
			want: Decision{Time: at, Kind: "NET", Binary: "/usr/bin/curl", Host: "93.184.216.34:443", Reason: "endpoint example.com:443 not in policy"},
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
		{name: "network activity other than open", body: `{"class_uid":4001,"activity_id":5,"action_id":2,"dst_endpoint":{"ip":"1.2.3.4","port":1}}`},
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
