package openshell

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	ocsfNetworkActivity = 4001
	ocsfHTTPActivity    = 4002
	ocsfActivityOpen    = 1
	ocsfActivityRefuse  = 5
	ocsfActionAllowed   = 1
	ocsfActionDenied    = 2
)

type ocsfEvent struct {
	ClassUID     int    `json:"class_uid"`
	ActivityID   int    `json:"activity_id"`
	ActionID     int    `json:"action_id"`
	Time         int64  `json:"time"` // milliseconds since epoch
	Message      string `json:"message"`
	StatusDetail string `json:"status_detail"`
	DstEndpoint  struct {
		Domain string `json:"domain"`
		IP     string `json:"ip"`
		Port   int    `json:"port"`
	} `json:"dst_endpoint"`
	Actor struct {
		Process struct {
			Name string `json:"name"`
		} `json:"process"`
	} `json:"actor"`
	FirewallRule struct {
		Name string `json:"name"`
	} `json:"firewall_rule"`
	Device struct {
		Container struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"container"`
	} `json:"device"`
	HTTPRequest struct {
		Method string `json:"http_method"`
		URL    struct {
			Hostname string `json:"hostname"`
			Path     string `json:"path"`
		} `json:"url"`
	} `json:"http_request"`
}

// ParseOCSF maps an OCSF event body to its Decision plus the sandbox name and ID from device.container; ok is false for
// anything that isn't a network decision, but the name and ID are still returned so other events can be attributed.
func ParseOCSF(body []byte) (d Decision, sandboxName, sandboxID string, ok bool) {
	var e ocsfEvent
	if json.Unmarshal(body, &e) != nil {
		return Decision{}, "", "", false
	}
	d, ok = decisionFromEvent(e)
	if !ok {
		d = Decision{}
	}
	return d, e.Device.Container.Name, e.Device.Container.UID, ok
}

// decisionFromOCSF maps an OCSF event body to the Decision parseDecision yields for the same event; it reports false for anything that isn't a network decision.
func decisionFromOCSF(body []byte) (Decision, bool) {
	d, _, _, ok := ParseOCSF(body)
	return d, ok
}

func decisionFromEvent(e ocsfEvent) (Decision, bool) {
	if e.ActionID != ocsfActionAllowed && e.ActionID != ocsfActionDenied {
		return Decision{}, false
	}
	d := Decision{Time: time.UnixMilli(e.Time).UTC(), Allowed: e.ActionID == ocsfActionAllowed, Policy: e.FirewallRule.Name}
	switch {
	case e.ClassUID == ocsfNetworkActivity && (e.ActivityID == ocsfActivityOpen || e.ActivityID == ocsfActivityRefuse):
		host := e.DstEndpoint.Domain
		if host == "" {
			host = e.DstEndpoint.IP
		}
		if host == "" {
			return Decision{}, false
		}
		d.Kind, d.Activity, d.Binary, d.Host = "NET", "OPEN", e.Actor.Process.Name, host
		if e.ActivityID == ocsfActivityRefuse {
			d.Activity = "REFUSE"
		}
		if e.DstEndpoint.Port != 0 {
			d.Host += ":" + strconv.Itoa(e.DstEndpoint.Port)
		}
	case e.ClassUID == ocsfHTTPActivity:
		path, _, _ := strings.Cut(e.HTTPRequest.URL.Path, "?")
		d.Kind, d.Method, d.Host, d.Path = "HTTP", e.HTTPRequest.Method, e.HTTPRequest.URL.Hostname, path
	default:
		return Decision{}, false
	}
	if !d.Allowed {
		d.Reason = e.StatusDetail
		if d.Reason == "" {
			d.Reason = e.Message
		}
	}
	return d, true
}
