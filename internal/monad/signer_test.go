package monad

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../tests/fixtures/signer-v0.16.1.out")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseSignerOutputFixture(t *testing.T) {
	s, warn, err := ParseSignerOutput(fixture(t), "2")
	if err != nil {
		t.Fatal(err)
	}
	if warn != "" {
		t.Errorf("unexpected warning %q", warn)
	}
	if s.Address != "203.0.113.7:8000" || s.AuthPort != "8001" || s.Seq != "2" ||
		!strings.HasPrefix(s.Sig, "15e06d24aebe") || len(s.Sig) != 130 {
		t.Errorf("parsed %+v", s)
	}
}

func TestParseSignerOutputDrift(t *testing.T) {
	out := fixture(t)
	var f *ui.Fatal
	// lower than requested: hard stop
	if _, _, err := ParseSignerOutput(out, "3"); !errors.As(err, &f) || !strings.Contains(f.Msg, "lower than the requested 3") {
		t.Errorf("lower seq: %v", err)
	}
	// higher than requested: warning, continue
	if _, warn, err := ParseSignerOutput(out, "1"); err != nil || !strings.Contains(warn, "Signer emitted seq 2 (requested 1)") {
		t.Errorf("higher seq: %v %q", err, warn)
	}
}

func TestParseSignerOutputRefusals(t *testing.T) {
	out := fixture(t)
	var f *ui.Fatal
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"tcp/udp differ", strings.Replace(out, "self_udp_port = 8000", "self_udp_port = 8002", 1), "different TCP and UDP ports (8000 / 8002)"},
		{"missing udp", strings.Replace(out, "self_udp_port = 8000\n", "", 1), "unusable self_udp_port"},
		{"missing auth", strings.Replace(out, "self_auth_port = 8001\n", "", 1), "unusable self_auth_port"},
		{"address with port", strings.Replace(out, `"203.0.113.7"`, `"203.0.113.7:8000"`, 1), "not a plain IPv4 address"},
		{"missing sig", strings.Replace(out, "self_name_record_sig", "other", 1), "self_name_record_sig"},
		{"missing seq", strings.Replace(out, "self_record_seq_num", "other", 1), "self_record_seq_num"},
		{"empty", "", "not a plain IPv4 address"},
	}
	for _, tc := range cases {
		_, _, err := ParseSignerOutput(tc.out, "2")
		if !errors.As(err, &f) || !strings.Contains(f.Msg, tc.want) {
			t.Errorf("%s: got %v, want message containing %q", tc.name, err, tc.want)
		}
	}
}
