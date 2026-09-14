// Package foundation reads the Monad Foundation validator snapshot to suggest
// a name-record sequence. The value is a suggestion the operator can
// override; the snapshot never decides anything on its own. Matching is on
// the exact SECP public key of the key just imported, never on a name.
package foundation

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/s0urledd/monad-failover-tool/internal/netinfo"
)

// SeqSaneMax is the largest sequence that survives a JSON double intact.
const SeqSaneMax = 9007199254740991

const maxBody = 33554432 // 32 MiB cap on the snapshot body

// Result is a usable sequence from the snapshot.
type Result struct {
	Seq      uint64
	AgeHours int64
}

type snapshot struct {
	Network        string          `json:"network"`
	ChainID        json.RawMessage `json:"chain_id"`
	FetchedAtEpoch json.RawMessage `json:"fetched_at_epoch"`
	Validators     []validator     `json:"validators"`
}

type validator struct {
	Secp string `json:"secp"`
	Bls  string `json:"bls"`
	Peer *peer  `json:"peer"`
}

type peer struct {
	RecordSeqNum json.RawMessage `json:"record_seq_num"`
}

// rawScalar renders a JSON string or number as text, "" for null/absent.
func rawScalar(r json.RawMessage) string {
	s := strings.TrimSpace(string(r))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if json.Unmarshal(r, &v) == nil {
			return v
		}
		return ""
	}
	return s
}

var (
	epochRe = regexp.MustCompile(`^[0-9]{1,12}$`)
	seqRe   = regexp.MustCompile(`^[0-9]{1,16}$`)
)

func normKey(k string) string {
	return strings.TrimPrefix(strings.ToLower(k), "0x")
}

// Fetch downloads the snapshot for network from base. "" body means failure.
func Fetch(base, network string) ([]byte, error) {
	resp, err := netinfo.Client(30 * time.Second).Get(base + "/" + network + ".json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("snapshot returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

// Lookup returns the last published sequence for secp, or a short note
// saying why the snapshot could not be used. now is injectable for tests.
func Lookup(body []byte, fetchErr error, network, secp, bls string, maxAge time.Duration, now time.Time) (Result, string) {
	var wantChain string
	switch network {
	case "mainnet":
		wantChain = "143"
	case "testnet":
		wantChain = "10143"
	default:
		return Result{}, "unknown network '" + network + "'"
	}
	if fetchErr != nil || len(body) == 0 {
		return Result{}, "snapshot unreachable"
	}

	// A truncated document must not yield a sequence: encoding/json refuses
	// anything that is not one complete value.
	var snap snapshot
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&snap); err != nil {
		return Result{}, "snapshot is not complete JSON"
	}
	if _, err := dec.Token(); err != io.EOF {
		return Result{}, "snapshot is not complete JSON"
	}

	if snap.Network != network {
		n := snap.Network
		if n == "" {
			n = "unknown"
		}
		return Result{}, "snapshot is for '" + n + "', not " + network
	}
	if chain := rawScalar(snap.ChainID); chain != wantChain {
		if chain == "" {
			chain = "unknown"
		}
		return Result{}, "snapshot chain_id is '" + chain + "', expected " + wantChain
	}
	fetched := rawScalar(snap.FetchedAtEpoch)
	if !epochRe.MatchString(fetched) {
		return Result{}, "snapshot has no usable timestamp"
	}
	fe, _ := strconv.ParseInt(fetched, 10, 64)
	age := now.Unix() - fe
	if age < 0 {
		age = 0
	}
	if time.Duration(age)*time.Second > maxAge {
		return Result{}, fmt.Sprintf("snapshot is %dh old", age/3600)
	}

	var hits []validator
	for _, v := range snap.Validators {
		if strings.EqualFold(v.Secp, secp) {
			hits = append(hits, v)
		}
	}
	if len(hits) != 1 {
		return Result{}, fmt.Sprintf("%d entries matched this key", len(hits))
	}
	hit := hits[0]

	// Both keys must agree, or this entry is not this validator. An entry
	// with no BLS is not good enough to act on.
	if normKey(hit.Bls) == "" {
		return Result{}, "snapshot entry has no BLS key to check against"
	}
	if normKey(hit.Bls) != normKey(bls) {
		return Result{}, "BLS key does not match the snapshot entry"
	}

	// No peer record published is NOT sequence zero; it means unknown.
	if hit.Peer == nil {
		return Result{}, "no name record published for this key yet"
	}
	seqText := rawScalar(hit.Peer.RecordSeqNum)
	if seqText == "" {
		return Result{}, "no name record published for this key yet"
	}
	if !seqRe.MatchString(seqText) {
		return Result{}, "sequence out of usable range"
	}
	seq, err := strconv.ParseUint(seqText, 10, 64)
	if err != nil || seq > SeqSaneMax {
		return Result{}, "sequence out of usable range"
	}
	return Result{Seq: seq, AgeHours: age / 3600}, ""
}
