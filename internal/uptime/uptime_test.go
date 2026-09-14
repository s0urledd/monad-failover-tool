package uptime

import (
	"testing"

	"github.com/s0urledd/monad-failover-tool/internal/testutil"
)

func TestParse(t *testing.T) {
	rep, ok := Parse([]byte(testutil.UptimeActive(testutil.MockSecp)))
	if !ok || rep.Name != "MockVal" || rep.Status != "active" || rep.UptimePercent != "100" ||
		rep.Finalized != "356" || rep.Timeout != "0" || rep.LastRound != "87138952" {
		t.Errorf("healthy: ok=%v %+v", ok, rep)
	}

	rep, ok = Parse([]byte(`{"success":true,"uptime":{"validator_name":"MockVal","status":"inactive","uptime_percent":0,"finalized_count":0,"timeout_count":0,"last_round":null}}`))
	if !ok || rep.Status != "inactive" || rep.LastRound != "" || rep.UptimePercent != "0" {
		t.Errorf("null last_round: ok=%v %+v", ok, rep)
	}

	rep, ok = Parse([]byte(`{"success":true,"uptime":{}}`))
	if !ok || rep != (Report{}) {
		t.Errorf("empty uptime: ok=%v %+v", ok, rep)
	}

	if _, ok = Parse([]byte(`{"success":false}`)); ok {
		t.Error("success=false accepted")
	}
	if _, ok = Parse([]byte(`not json`)); ok {
		t.Error("garbage accepted")
	}
	if _, ok = Parse([]byte(``)); ok {
		t.Error("empty accepted")
	}
	// fields at the top level are found too
	rep, ok = Parse([]byte(`{"success": true, "status": "active", "uptime_percent": 99.5}`))
	if !ok || rep.Status != "active" || rep.UptimePercent != "99.5" {
		t.Errorf("top-level fields: ok=%v %+v", ok, rep)
	}
}
