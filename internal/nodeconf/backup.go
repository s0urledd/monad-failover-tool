package nodeconf

import (
	"bytes"
	"os"
	"regexp"

	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

var ikmRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// ValidateIKM strips an optional 0x prefix and accepts exactly 64 hex
// digits, returned as a fresh buffer the caller zeroes after use. The
// input is left for the caller to zero as well.
func ValidateIKM(s []byte) ([]byte, bool) {
	s = bytes.TrimPrefix(s, []byte("0x"))
	if !ikmRe.Match(s) {
		return nil, false
	}
	out := make([]byte, len(s))
	copy(out, s)
	return out, true
}

// ExtractIKMFromBackup returns the secret from an official key backup file,
// the format `monad-keystore recover` writes: the last field of the first
// "Keystore secret:" or "Keep your IKM secure:" line. The file buffer is
// zeroed before returning. nil when absent.
func ExtractIKMFromBackup(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	defer ui.Zero(data)
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.Contains(line, []byte("Keystore secret:")) && !bytes.Contains(line, []byte("Keep your IKM secure:")) {
			continue
		}
		fields := bytes.Fields(bytes.ReplaceAll(line, []byte("\r"), nil))
		if len(fields) == 0 {
			return nil
		}
		last := fields[len(fields)-1]
		out := make([]byte, len(last))
		copy(out, last)
		return out
	}
	return nil
}
