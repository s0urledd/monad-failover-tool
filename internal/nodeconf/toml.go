package nodeconf

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// Editing is line based and restricted to the requested table. node.toml is
// a small, flat file the tool also writes, so a full TOML implementation is
// not needed and would be one more dependency to audit. Unrelated tables are
// passed through untouched.

var headerRe = regexp.MustCompile(`^\s*\[`)

func tableHeader(line string) string {
	h := line
	if i := strings.Index(h, "#"); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSpace(h)
}

// SetTomlValue sets key = value inside [section] (or at the root when
// section is ""). The value is written verbatim, so quote strings yourself.
// The key must occur at most once in that table; a missing key is appended
// to the table (never to the root). Anything else is refused: the file is
// rewritten only when the edit is unambiguous.
func SetTomlValue(file, key, value, section string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return ui.Die("Cannot read " + file)
	}
	keyRe := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	repl := key + " = " + value

	active := section == ""
	seen := active
	found := 0
	var out []string
	for _, line := range lines {
		if headerRe.MatchString(line) {
			if active && found == 0 && section != "" {
				out = append(out, repl)
				found = 1
			}
			active = section != "" && tableHeader(line) == "["+section+"]"
			if active {
				seen = true
			}
			out = append(out, line)
			continue
		}
		if active && keyRe.MatchString(line) {
			out = append(out, repl)
			found++
			continue
		}
		out = append(out, line)
	}
	if active && found == 0 && section != "" {
		out = append(out, repl)
		found = 1
	}
	if !seen || found != 1 {
		label := section
		if label == "" {
			label = "root"
		}
		return ui.Die("Cannot uniquely set '" + key + "' in " + label + " of " + file + ".")
	}
	return writeAtomic(file, strings.Join(out, "\n")+"\n")
}

// writeAtomic replaces file through a temporary sibling and rename, keeping
// the original mode. Used only on staging copies the tool created itself.
func writeAtomic(file, content string) error {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(file); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".edit.*")
	if err != nil {
		return ui.Die("Cannot write " + file)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(name)
		return ui.Die("Cannot write " + file)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return ui.Die("Cannot write " + file)
	}
	_ = os.Chmod(name, mode)
	if err := os.Rename(name, file); err != nil {
		os.Remove(name)
		return ui.Die("Cannot write " + file)
	}
	return nil
}

// TomlGet returns the first `key = value` in the file, unquoted, "" when
// absent. It is used only for values this tool also writes at the root.
func TomlGet(file, key string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	keyRe := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	for _, line := range strings.Split(string(data), "\n") {
		if !keyRe.MatchString(line) {
			continue
		}
		v := line[strings.Index(line, "=")+1:]
		v = strings.TrimLeft(v, " \t")
		v = strings.TrimPrefix(v, `"`)
		v = strings.TrimRight(v, " \t")
		v = strings.TrimSuffix(v, `"`)
		return v
	}
	return ""
}

// TomlValue reads key from the file using TOML-ish rules for a bare
// `key = value` line: the first match anywhere in the file.
func TomlValue(file, key string) string { return TomlGet(file, key) }

var (
	phAddrRe = regexp.MustCompile(`^self_address\s*=\s*"<`)
	phSeqRe  = regexp.MustCompile(`^self_record_seq_num\s*=\s*[^0-9]`)
	phSigRe  = regexp.MustCompile(`^self_name_record_sig\s*=\s*"(<|[^0-9a-fA-F])`)
)

// SanitizePlaceholders replaces install-time placeholders so the signer's
// output can be patched over well-formed values.
func SanitizePlaceholders(file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return ui.Die("Cannot read " + file)
	}
	zeroSig := strings.Repeat("0", 130)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	changed := false
	for i, line := range lines {
		switch {
		case phAddrRe.MatchString(line):
			lines[i] = `self_address = "0.0.0.0:8000"`
			changed = true
		case phSeqRe.MatchString(line):
			lines[i] = `self_record_seq_num = 0`
			changed = true
		case phSigRe.MatchString(line):
			lines[i] = `self_name_record_sig = "` + zeroSig + `"`
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writeAtomic(file, strings.Join(lines, "\n")+"\n")
}

// MissingConfigFlags lists which of the three migration flags are not set to
// true at the start of a line, in the order the docs name them.
func MissingConfigFlags(file string) []string {
	data, _ := os.ReadFile(file)
	text := string(data)
	var missing []string
	for _, k := range []string{"enable_publisher", "enable_client", "expand_to_group"} {
		if !strings.Contains("\n"+text, "\n"+k+" = true") {
			missing = append(missing, k)
		}
	}
	return missing
}

// Beneficiary and node_name shapes.
var (
	BeneficiaryRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	ZeroAddressRe = regexp.MustCompile(`^0x0{40}$`)
	NodeNameRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)
