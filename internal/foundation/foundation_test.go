package foundation

import (
	"errors"
	"testing"
	"time"

	"github.com/s0urledd/monad-failover-tool/internal/testutil"
)

const (
	secp = testutil.MockSecp
	bls  = testutil.MockBls
)

func TestLookupGuards(t *testing.T) {
	now := time.Now()
	day := 86400 * time.Second
	cases := []struct {
		name     string
		body     string
		err      error
		network  string
		secp     string
		bls      string
		wantSeq  uint64
		wantNote string
	}{
		{"published sequence", testutil.Snapshot(testutil.SnapshotOpts{}), nil, "testnet", secp, bls, 7, ""},
		{"case-insensitive key match", testutil.Snapshot(testutil.SnapshotOpts{}), nil, "testnet", "0xsecp1111111111111111111111111111111111111111", bls, 7, ""},
		{"reordered fields", testutil.Snapshot(testutil.SnapshotOpts{Reorder: true}), nil, "testnet", secp, bls, 7, ""},
		{"unreachable", "", errors.New("dial"), "testnet", secp, bls, 0, "snapshot unreachable"},
		{"empty body", "", nil, "testnet", secp, bls, 0, "snapshot unreachable"},
		{"truncated", testutil.Snapshot(testutil.SnapshotOpts{Truncate: true}), nil, "testnet", secp, bls, 0, "snapshot is not complete JSON"},
		{"trailing garbage", testutil.Snapshot(testutil.SnapshotOpts{}) + "{}", nil, "testnet", secp, bls, 0, "snapshot is not complete JSON"},
		{"other network", testutil.Snapshot(testutil.SnapshotOpts{Network: "mainnet"}), nil, "testnet", secp, bls, 0, "snapshot is for 'mainnet', not testnet"},
		{"wrong chain id", testutil.Snapshot(testutil.SnapshotOpts{ChainID: "999"}), nil, "testnet", secp, bls, 0, "snapshot chain_id is '999', expected 10143"},
		{"stale", testutil.Snapshot(testutil.SnapshotOpts{Stale: true}), nil, "testnet", secp, bls, 0, "snapshot is 240h old"},
		{"duplicate entries", testutil.Snapshot(testutil.SnapshotOpts{Dup: true}), nil, "testnet", secp, bls, 0, "2 entries matched this key"},
		{"unknown key", testutil.Snapshot(testutil.SnapshotOpts{}), nil, "testnet", "0xNOBODY", bls, 0, "0 entries matched this key"},
		{"bls mismatch", testutil.Snapshot(testutil.SnapshotOpts{BadBLS: true}), nil, "testnet", secp, bls, 0, "BLS key does not match the snapshot entry"},
		{"no bls", testutil.Snapshot(testutil.SnapshotOpts{NoBLS: true}), nil, "testnet", secp, bls, 0, "snapshot entry has no BLS key to check against"},
		{"no peer record is not zero", testutil.Snapshot(testutil.SnapshotOpts{NoPeer: true}), nil, "testnet", secp, bls, 0, "no name record published for this key yet"},
		{"sequence too large", testutil.Snapshot(testutil.SnapshotOpts{Seq: "99999999999999999"}), nil, "testnet", secp, bls, 0, "sequence out of usable range"},
		{"sequence above sane max", testutil.Snapshot(testutil.SnapshotOpts{Seq: "9007199254740992"}), nil, "testnet", secp, bls, 0, "sequence out of usable range"},
		{"unknown network", testutil.Snapshot(testutil.SnapshotOpts{}), nil, "devnet", secp, bls, 0, "unknown network 'devnet'"},
		{"mainnet chain id", testutil.Snapshot(testutil.SnapshotOpts{Network: "mainnet"}), nil, "mainnet", secp, bls, 7, ""},
	}
	for _, tc := range cases {
		res, note := Lookup([]byte(tc.body), tc.err, tc.network, tc.secp, tc.bls, day, now)
		if note != tc.wantNote || res.Seq != tc.wantSeq {
			t.Errorf("%s: got seq=%d note=%q, want seq=%d note=%q", tc.name, res.Seq, note, tc.wantSeq, tc.wantNote)
		}
	}
}

// The real bucket document: chain_id is a JSON string, keys carry no 0x
// prefix, record_seq_num is a number and validators outside the active set
// have "peer": null.
func TestLookupRealShape(t *testing.T) {
	body := `{"chain_id": "143", "count": 2, "expected_version": "0.16.2", "fetched_at_epoch": ` +
		itoa(time.Now().Unix()) + `, "network": "mainnet", "validators": [` +
		`{"id": 1, "name": "A", "secp": "03aa", "bls": "96bb", "cohort": 1, "peer": {"address": "1.2.3.4", "auth_port": 8001, "name_record_sig": "x", "record_seq_num": 8, "tcp_port": 8000, "udp_port": 8000}},` +
		`{"id": 2, "name": "B", "secp": "02cc", "bls": "96dd", "peer": null}]}`
	res, note := Lookup([]byte(body), nil, "mainnet", "03aa", "0x96BB", 86400*time.Second, time.Now())
	if note != "" || res.Seq != 8 {
		t.Errorf("active validator: seq=%d note=%q", res.Seq, note)
	}
	_, note = Lookup([]byte(body), nil, "mainnet", "02cc", "96dd", 86400*time.Second, time.Now())
	if note != "no name record published for this key yet" {
		t.Errorf("peer null: note=%q", note)
	}
}

func itoa(n int64) string { return formatInt(n) }

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
