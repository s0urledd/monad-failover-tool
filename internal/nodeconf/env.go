// Package nodeconf reads and edits the node's own configuration files:
// the .env holding the keystore password, node.toml, and the official key
// backup format.
package nodeconf

import (
	"bufio"
	"os"
	"strings"
)

// LoadKeystorePassword reads KEYSTORE_PASSWORD from the .env file without
// sourcing it. The file is owned by the monad account; executing it as root
// would run anything a compromised account placed there. Only the one value
// is needed. Returns "" when the variable is absent.
func LoadKeystorePassword(envFile string) string {
	f, err := os.Open(envFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "KEYSTORE_PASSWORD=") {
			continue
		}
		val := strings.TrimPrefix(line, "KEYSTORE_PASSWORD=")
		// A .env saved with CRLF endings carries a trailing \r that would
		// defeat the quote strip and end up inside the password.
		val = strings.TrimSuffix(val, "\r")
		switch {
		case len(val) >= 2 && strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'"):
			val = val[1 : len(val)-1]
		case len(val) >= 2 && strings.HasPrefix(val, `"`) && strings.HasSuffix(val, `"`):
			val = val[1 : len(val)-1]
		}
		return val
	}
	return ""
}
