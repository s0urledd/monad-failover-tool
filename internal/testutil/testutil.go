// Package testutil builds the fixtures the test suite shares: Foundation
// snapshots and uptime responses shaped like the ones the shell release's
// mock curl produced, so every guard can be driven from a table.
package testutil

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// MockSecp and MockBls are the public keys the mock monad-keystore reports
// for the test suite's validator IKMs.
const (
	MockSecp = "0xSECP1111111111111111111111111111111111111111"
	MockBls  = "0xBLS2222222222222222222222222222222222222222"
	SecpIKM  = "1111111111111111111111111111111111111111111111111111111111111111"
	BlsIKM   = "2222222222222222222222222222222222222222222222222222222222222222"
)

// SnapshotOpts mirror the knobs of the shell suite's mock curl.
type SnapshotOpts struct {
	Network  string // default testnet
	ChainID  string // default by network
	Stale    bool   // fetched far past the freshness limit
	NoPeer   bool   // validator present, no name record
	Dup      bool   // same secp listed twice
	BadBLS   bool   // entry whose BLS does not match
	NoBLS    bool   // entry without a BLS key
	Reorder  bool   // peer before secp
	Truncate bool   // cut off after the target object closes
	Seq      string // default 7
	Secp     string // default MockSecp
	Bls      string // default MockBls
}

// Snapshot renders a validator snapshot document.
func Snapshot(o SnapshotOpts) string {
	net := o.Network
	if net == "" {
		net = "testnet"
	}
	chain := o.ChainID
	if chain == "" {
		if net == "mainnet" {
			chain = "143"
		} else {
			chain = "10143"
		}
	}
	epoch := time.Now().Unix()
	if o.Stale {
		epoch -= 864000
	}
	seq := o.Seq
	if seq == "" {
		seq = "7"
	}
	secp := o.Secp
	if secp == "" {
		secp = MockSecp
	}
	bls := o.Bls
	if bls == "" {
		bls = MockBls
	}
	if o.BadBLS {
		bls = "0xBLSdifferentkey0000000000000000000000000000"
	}
	if o.NoBLS {
		bls = ""
	}
	peer := fmt.Sprintf(`, "peer": {"address": "198.51.100.5", "tcp_port": 8000, "udp_port": 8000, "auth_port": 8001, "record_seq_num": %s, "name_record_sig": "0xdead"}`, seq)
	if o.NoPeer {
		peer = ""
	}
	var entry string
	if o.Reorder {
		entry = fmt.Sprintf(`{"id": 1, "name": "MockVal"%s, "secp": "%s", "bls": "%s"}`, peer, secp, bls)
	} else {
		entry = fmt.Sprintf(`{"id": 1, "name": "MockVal", "secp": "%s", "bls": "%s"%s}`, secp, bls, peer)
	}
	other := `{"id": 2, "name": "Other", "secp": "0xSECPffffffffffffffffffffffffffffffffffffff", "bls": "0xBLSf", "peer": {"record_seq_num": 3}}`
	body := entry + ", " + other
	if o.Dup {
		body = entry + ", " + entry
	}
	if o.Truncate {
		return fmt.Sprintf(`{"chain_id": "%s", "count": 2, "expected_version": "0.16.1", "fetched_at_epoch": %d, "network": "%s", "validators": [%s, {"id": 3, "secp": "0xcut`,
			chain, epoch, net, entry)
	}
	return fmt.Sprintf(`{"chain_id": "%s", "count": 2, "expected_version": "0.16.1", "fetched_at_epoch": %d, "network": "%s", "validators": [%s]}`,
		chain, epoch, net, body)
}

// UptimeActive is the default healthy uptime response for secp.
func UptimeActive(secp string) string {
	return fmt.Sprintf(`{"success": true, "uptime": {"validator_name": "MockVal", "secp_address": "%s", "status": "active", "window_hours": 24, "finalized_count": 356, "timeout_count": 0, "uptime_percent": 100, "last_round": 87138952}}`, secp)
}

// Endpoints is one HTTP server standing in for ifconfig.me, the Foundation
// bucket and the uptime API. Each field can be changed between requests.
type Endpoints struct {
	Server *httptest.Server

	IP       string // "" answers 500
	Snapshot string // "" answers 500
	Uptime   string // "" answers 500
}

// NewEndpoints starts the server with healthy defaults.
func NewEndpoints() *Endpoints {
	e := &Endpoints{IP: "203.0.113.7", Snapshot: Snapshot(SnapshotOpts{}), Uptime: UptimeActive(MockSecp)}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch {
		case r.URL.Path == "/ip":
			body = e.IP
		case strings.HasSuffix(r.URL.Path, ".json"):
			body = e.Snapshot
		case strings.HasPrefix(r.URL.Path, "/uptime/"):
			body = e.Uptime
		}
		if body == "" {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	return e
}

// IPURL, FoundationBase and UptimeBase are the values for the tool's endpoint overrides.
func (e *Endpoints) IPURL() string          { return e.Server.URL + "/ip" }
func (e *Endpoints) FoundationBase() string { return e.Server.URL }
func (e *Endpoints) UptimeBase() string     { return e.Server.URL + "/uptime" }

// Close stops the server.
func (e *Endpoints) Close() { e.Server.Close() }
