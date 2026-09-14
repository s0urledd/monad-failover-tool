package nodeconf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadKeystorePassword(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"KEYSTORE_PASSWORD='testpass'\n", "testpass"},
		{"KEYSTORE_PASSWORD=\"testpass\"\n", "testpass"},
		{"KEYSTORE_PASSWORD=plain\n", "plain"},
		{"KEYSTORE_PASSWORD='testpass'\r\n", "testpass"},
		{"OTHER=1\nKEYSTORE_PASSWORD='second'\n", "second"},
		{"# KEYSTORE_PASSWORD='commented'\n", ""},
		{"KEYSTORE_PASSWORD=\n", ""},
		{"", ""},
		{"KEYSTORE_PASSWORD='it''s'\n", "it''s"},
		{"KEYSTORE_PASSWORD=$(touch /tmp/pwned)\n", "$(touch /tmp/pwned)"},
	} {
		p := write(t, ".env", tc.env)
		if got := string(LoadKeystorePassword(p)); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.env, got, tc.want)
		}
	}
	if got := string(LoadKeystorePassword("/nonexistent/.env")); got != "" {
		t.Errorf("missing file: got %q", got)
	}
}

func TestValidateIKM(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{hex64, hex64, true},
		{"0x" + hex64, hex64, true},
		{strings.ToUpper(hex64), strings.ToUpper(hex64), true},
		{hex64[:63], "", false},
		{hex64 + "a", "", false},
		{"", "", false},
		{"zz" + hex64[2:], "", false},
	} {
		got, ok := ValidateIKM([]byte(tc.in))
		if ok != tc.ok || string(got) != tc.want {
			t.Errorf("ValidateIKM(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestExtractIKMFromBackup(t *testing.T) {
	hex64 := strings.Repeat("11", 32)
	for _, tc := range []struct{ body, want string }{
		{"Secp public key: 0xSECP\nKeystore secret: " + hex64 + "\n", hex64},
		{"Keep your IKM secure: " + hex64 + "\n", hex64},
		{"Keystore secret: " + hex64 + "\r\n", hex64},
		{"Keystore secret: first\nKeystore secret: second\n", "first"},
		{"no secret here\n", ""},
		{"", ""},
	} {
		p := write(t, "backup", tc.body)
		if got := string(ExtractIKMFromBackup(p)); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.body, got, tc.want)
		}
	}
}

const fixture = `network_name = "testnet"
node_name = "fullnode-one"
beneficiary = "0x0000000000000000000000000000000000000000"

[peer_discovery]
self_address = "<IP>:<PORT>"
self_auth_port = 8001
self_record_seq_num = 0
self_name_record_sig = "<NAME_RECORD_SIG>"

[fullnode_raptorcast]
enable_publisher = false
enable_client = false

[statesync]
expand_to_group = false
`

func TestSetTomlValueRootAndTables(t *testing.T) {
	p := write(t, "node.toml", fixture)
	if err := SetTomlValue(p, "beneficiary", `"0xBEEF00000000000000000000000000000000BEEF"`, ""); err != nil {
		t.Fatal(err)
	}
	if err := SetTomlValue(p, "self_address", `"203.0.113.7:8000"`, "peer_discovery"); err != nil {
		t.Fatal(err)
	}
	if err := SetTomlValue(p, "enable_client", "true", "fullnode_raptorcast"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	for _, want := range []string{
		`beneficiary = "0xBEEF00000000000000000000000000000000BEEF"`,
		`self_address = "203.0.113.7:8000"`,
		"enable_client = true",
		"enable_publisher = false", // untouched sibling
		"expand_to_group = false",  // untouched table
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Count(s, "beneficiary =") != 1 || strings.Count(s, "enable_client =") != 1 {
		t.Errorf("duplicated keys:\n%s", s)
	}
}

func TestSetTomlValueInsertsMissingKeyIntoItsTable(t *testing.T) {
	p := write(t, "node.toml", strings.ReplaceAll(fixture, "enable_client = false\n", ""))
	if err := SetTomlValue(p, "enable_client", "true", "fullnode_raptorcast"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	lines := strings.Split(string(got), "\n")
	// the key must land inside [fullnode_raptorcast], before [statesync]
	var inRaptor bool
	found := false
	for _, l := range lines {
		if strings.HasPrefix(l, "[") {
			inRaptor = l == "[fullnode_raptorcast]"
		}
		if l == "enable_client = true" {
			found = inRaptor
		}
	}
	if !found {
		t.Errorf("enable_client not inserted into [fullnode_raptorcast]:\n%s", got)
	}
	// last table: appended at end of file
	p2 := write(t, "node.toml", strings.ReplaceAll(fixture, "expand_to_group = false\n", ""))
	if err := SetTomlValue(p2, "expand_to_group", "true", "statesync"); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(p2)
	if !strings.HasSuffix(string(got2), "[statesync]\nexpand_to_group = true\n") {
		t.Errorf("expand_to_group not appended to [statesync]:\n%s", got2)
	}
}

func TestSetTomlValueRefusesAmbiguity(t *testing.T) {
	var f *ui.Fatal
	// duplicate root key
	p := write(t, "node.toml", strings.Replace(fixture, "beneficiary = ", "beneficiary = \"0x0\"\nbeneficiary = ", 1))
	err := SetTomlValue(p, "beneficiary", `"0xBEEF00000000000000000000000000000000BEEF"`, "")
	if !errors.As(err, &f) || !strings.Contains(f.Msg, "Cannot uniquely set 'beneficiary' in root") {
		t.Errorf("duplicate root key: got %v", err)
	}
	// missing root key is never inserted
	p = write(t, "node.toml", strings.ReplaceAll(fixture, "node_name = \"fullnode-one\"\n", ""))
	if err := SetTomlValue(p, "node_name", `"x"`, ""); !errors.As(err, &f) {
		t.Errorf("missing root key: got %v", err)
	}
	// missing table
	p = write(t, "node.toml", fixture)
	if err := SetTomlValue(p, "x", "1", "nonexistent"); !errors.As(err, &f) {
		t.Errorf("missing table: got %v", err)
	}
	// the file is untouched after a refusal
	got, _ := os.ReadFile(p)
	if string(got) != fixture {
		t.Errorf("file changed after refusal")
	}
}

func TestSetTomlValueScopesToTableWithCommentedHeader(t *testing.T) {
	p := write(t, "node.toml", fixture+"\n[unrelated]  # a comment\nenable_client = false\n")
	if err := SetTomlValue(p, "enable_client", "true", "fullnode_raptorcast"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "[unrelated]  # a comment\nenable_client = false\n") {
		t.Errorf("unrelated table changed:\n%s", got)
	}
	if err := SetTomlValue(p, "enable_client", "true", "unrelated"); err != nil {
		t.Errorf("commented header not matched: %v", err)
	}
}

func TestTomlGet(t *testing.T) {
	p := write(t, "node.toml", fixture)
	if got := TomlGet(p, "beneficiary"); got != "0x0000000000000000000000000000000000000000" {
		t.Errorf("beneficiary: %q", got)
	}
	if got := TomlGet(p, "self_auth_port"); got != "8001" {
		t.Errorf("self_auth_port: %q", got)
	}
	if got := TomlGet(p, "missing"); got != "" {
		t.Errorf("missing: %q", got)
	}
}

func TestSanitizePlaceholders(t *testing.T) {
	p := write(t, "node.toml", fixture)
	if err := SanitizePlaceholders(p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	for _, want := range []string{
		`self_address = "0.0.0.0:8000"`,
		`self_name_record_sig = "` + strings.Repeat("0", 130) + `"`,
		"self_record_seq_num = 0",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// real values are left alone
	real := strings.NewReplacer(`"<IP>:<PORT>"`, `"1.2.3.4:8000"`, `"<NAME_RECORD_SIG>"`, `"abcd"`).Replace(fixture)
	p2 := write(t, "node.toml", real)
	if err := SanitizePlaceholders(p2); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(p2)
	if string(got2) != real {
		t.Errorf("real values changed:\n%s", got2)
	}
}

func TestMissingConfigFlags(t *testing.T) {
	p := write(t, "node.toml", fixture)
	if got := MissingConfigFlags(p); strings.Join(got, " ") != "enable_publisher enable_client expand_to_group" {
		t.Errorf("all false: %v", got)
	}
	all := strings.NewReplacer("enable_publisher = false", "enable_publisher = true",
		"enable_client = false", "enable_client = true",
		"expand_to_group = false", "expand_to_group = true").Replace(fixture)
	p = write(t, "node.toml", all)
	if got := MissingConfigFlags(p); len(got) != 0 {
		t.Errorf("all true: %v", got)
	}
}
