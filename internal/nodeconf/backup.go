package nodeconf

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

var ikmRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// ValidateIKM strips an optional 0x prefix and accepts exactly 64 hex digits.
func ValidateIKM(s string) (string, bool) {
	s = strings.TrimPrefix(s, "0x")
	if !ikmRe.MatchString(s) {
		return "", false
	}
	return s, true
}

// ExtractIKMFromBackup returns the secret from an official key backup file,
// the format `monad-keystore recover` writes: the last field of the first
// "Keystore secret:" or "Keep your IKM secure:" line. "" when absent.
func ExtractIKMFromBackup(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "Keystore secret:") || strings.Contains(line, "Keep your IKM secure:") {
			fields := strings.Fields(strings.ReplaceAll(line, "\r", ""))
			if len(fields) == 0 {
				return ""
			}
			return fields[len(fields)-1]
		}
	}
	return ""
}
