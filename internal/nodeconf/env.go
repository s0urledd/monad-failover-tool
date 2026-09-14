// Package nodeconf reads and edits the node's own configuration files:
// the .env holding the keystore password, node.toml, and the official key
// backup format.
package nodeconf

import (
	"bytes"
	"os"

	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// LoadKeystorePassword reads KEYSTORE_PASSWORD from the .env file without
// executing it. The file is owned by the monad account; executing it as
// root would run anything a compromised account placed there. Only the one
// value is needed. The file buffer is zeroed before returning; the caller
// zeroes the result when done. nil when the variable is absent.
func LoadKeystorePassword(envFile string) []byte {
	data, err := os.ReadFile(envFile)
	if err != nil {
		return nil
	}
	defer ui.Zero(data)
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("KEYSTORE_PASSWORD=")) {
			continue
		}
		val := bytes.TrimPrefix(line, []byte("KEYSTORE_PASSWORD="))
		// A .env saved with CRLF endings carries a trailing \r that would
		// defeat the quote strip and end up inside the password.
		val = bytes.TrimSuffix(val, []byte("\r"))
		switch {
		case len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'':
			val = val[1 : len(val)-1]
		case len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"':
			val = val[1 : len(val)-1]
		}
		if len(val) == 0 {
			return nil
		}
		out := make([]byte, len(val))
		copy(out, val)
		return out
	}
	return nil
}
