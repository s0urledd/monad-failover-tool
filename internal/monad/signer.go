package monad

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/s0urledd/monad-failover-tool/internal/netinfo"
	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// Signed is what the signer emitted, checked and assembled for node.toml.
// The signature is bound to the values the signer EMITS, so its output is
// the single source of truth for every patched field.
type Signed struct {
	Address  string // "ip:port", node.toml's combined form
	AuthPort string
	Seq      string
	Sig      string
}

var seqRe = regexp.MustCompile(`^[0-9]+$`)

func signerLine(out, key string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, key+" ") {
			return line, true
		}
	}
	return "", false
}

func quoted(line string) string {
	parts := strings.Split(line, `"`)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func lastField(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// ParseSignerOutput reads the v0.16.x signer format (one field per line,
// bare IP in self_address, separate port lines) and refuses anything that
// cannot be written to node.toml unambiguously. requested is the sequence
// passed to the signer; a lower emitted value is a hard stop, a higher one a
// warning the caller prints.
func ParseSignerOutput(out string, requested string) (Signed, string, error) {
	var s Signed
	get := func(key string) string {
		l, _ := signerLine(out, key)
		return l
	}
	ip := quoted(get("self_address"))
	tcp := lastField(get("self_tcp_port"))
	udp := lastField(get("self_udp_port"))
	auth := lastField(get("self_auth_port"))
	s.Sig = quoted(get("self_name_record_sig"))
	s.Seq = lastField(get("self_record_seq_num"))

	if !netinfo.ValidIPv4(ip) {
		return s, "", ui.Die("Signer emitted self_address='"+ip+"', which is not a plain IPv4 address.",
			"Nothing has been written. Check the installed monad version and re-run.")
	}
	if !netinfo.ValidPort(tcp) {
		return s, "", ui.Die("Signer emitted an unusable self_tcp_port ('" + tcp + "'). Nothing written.")
	}
	if !netinfo.ValidPort(udp) {
		return s, "", ui.Die("Signer emitted an unusable self_udp_port ('" + udp + "'). Nothing written.")
	}
	if !netinfo.ValidPort(auth) {
		return s, "", ui.Die("Signer emitted an unusable self_auth_port ('" + auth + "'). Nothing written.")
	}
	// node.toml holds a single port inside self_address, so the two must
	// agree; otherwise writing the combined form would silently drop one.
	if tcp != udp {
		return s, "", ui.Die("Signer emitted different TCP and UDP ports ("+tcp+" / "+udp+").",
			"self_address carries one port, so this cannot be written unambiguously.",
			"Nothing has been changed.")
	}
	s.Address = ip + ":" + tcp
	s.AuthPort = auth
	if s.Sig == "" {
		return s, "", ui.Die("Failed to parse self_name_record_sig from signer output")
	}
	if !seqRe.MatchString(s.Seq) {
		return s, "", ui.Die("Failed to parse self_record_seq_num from signer output")
	}

	// Emitting LESS than requested would re-create the stale-seq ghost-node
	// failure: hard stop. More (a +1 style version) is monotonic-safe.
	got, _ := strconv.ParseUint(s.Seq, 10, 64)
	want, _ := strconv.ParseUint(requested, 10, 64)
	warning := ""
	switch {
	case got < want:
		return s, "", ui.Die("Signer emitted seq "+s.Seq+", lower than the requested "+requested+".",
			"A stale seq would be rejected by peers. Check the monad version",
			"and re-run; nothing has been written.")
	case got != want:
		warning = "Signer emitted seq " + s.Seq + " (requested " + requested + ") — this monad version increments it."
	}
	return s, warning, nil
}
