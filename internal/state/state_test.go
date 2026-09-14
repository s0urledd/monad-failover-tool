package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s0urledd/monad-failover-tool/internal/netinfo"
	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

func newDir(t *testing.T) (Dir, string) {
	t.Helper()
	root := t.TempDir()
	d := Resolve(true, filepath.Join(root, "var", "lib", "monad-failover"))
	if err := d.Secure(os.Geteuid(), "/opt/monad/backup"); err != nil {
		t.Fatal(err)
	}
	return d, root
}

func fatalMsg(t *testing.T, err error, want string) {
	t.Helper()
	var f *ui.Fatal
	if !errors.As(err, &f) {
		t.Fatalf("expected a Fatal containing %q, got %v", want, err)
	}
	all := f.Msg + "\n" + strings.Join(f.Lines, "\n")
	if !strings.Contains(all, want) {
		t.Fatalf("expected %q in:\n%s", want, all)
	}
}

func TestResolveHonoursOverrideOnlyInSandbox(t *testing.T) {
	if got := Resolve(false, "/tmp/x").Root; got != DefaultDir {
		t.Errorf("override honoured outside sandbox: %s", got)
	}
	if got := Resolve(true, "/tmp/x").Root; got != "/tmp/x" {
		t.Errorf("override ignored in sandbox: %s", got)
	}
	if got := Resolve(true, "").Root; got != DefaultDir {
		t.Errorf("empty override: %s", got)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	d, _ := newDir(t)
	s := d.Store()
	if s.Exists() {
		t.Fatal("state exists before any write")
	}
	for _, kv := range [][2]string{{"last_step", "3"}, {"network", "testnet"}, {"last_step", "4"}, {"ip", "203.0.113.7"}} {
		if err := s.Set(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Get("last_step"); got != "4" {
		t.Errorf("last_step = %q", got)
	}
	if got := s.Get("network"); got != "testnet" {
		t.Errorf("network = %q", got)
	}
	if got := s.Get("missing"); got != "" {
		t.Errorf("missing = %q", got)
	}
	// a value containing "=" survives intact
	if err := s.Set("odd", "a=b=c"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("odd"); got != "a=b=c" {
		t.Errorf("odd = %q", got)
	}
	fi, err := os.Stat(d.File)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode: %v %v", fi.Mode(), err)
	}
	if !s.CompletedStep(4) || s.CompletedStep(5) {
		t.Error("CompletedStep wrong")
	}
	// no temporary files left behind
	entries, _ := os.ReadDir(d.Root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state.") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	s.Clear()
	if s.Exists() {
		t.Error("state exists after Clear")
	}
}

func TestDuplicateKeyIsRefusedNotFirstMatched(t *testing.T) {
	d, _ := newDir(t)
	s := d.Store()
	os.WriteFile(d.File, []byte("last_step=3\nlast_step=7\n"), 0o600)
	if got := s.Get("last_step"); got != "3\n7" {
		t.Errorf("duplicate key read as %q", got)
	}
	if s.CompletedStep(3) {
		t.Error("duplicated last_step satisfied CompletedStep")
	}
	fatalMsg(t, s.ValidateConsistency("/opt/monad/backup"), "'last_step' holds an unexpected value")
}

func TestSecureRefusesSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	os.MkdirAll(target, 0o700)
	link := filepath.Join(root, "state-link")
	os.Symlink(target, link)
	d := Resolve(true, link)
	fatalMsg(t, d.Secure(os.Geteuid(), "/b"), "is a symlink; refusing to use it.")
}

func TestSecureRefusesLooseDirectoryWithoutRepairing(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "loose")
	os.MkdirAll(p, 0o700)
	os.Chmod(p, 0o755)
	d := Resolve(true, p)
	fatalMsg(t, d.Secure(os.Geteuid(), "/b"), "has mode 755, not 700; refusing to use it.")
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o755 {
		t.Error("directory was repaired instead of refused")
	}
}

func TestSecureRefusesSymlinkedStateFileAndLeavesTargetAlone(t *testing.T) {
	d, root := newDir(t)
	victim := filepath.Join(root, "victim")
	os.WriteFile(victim, []byte("precious"), 0o600)
	os.Symlink(victim, d.File)
	fatalMsg(t, d.Secure(os.Geteuid(), "/b"), "is a symlink; refusing to read or write through it.")
	got, _ := os.ReadFile(victim)
	if string(got) != "precious" {
		t.Error("symlink target was written through")
	}
}

func TestSecureRefusesNonRegularStateFile(t *testing.T) {
	d, _ := newDir(t)
	os.Mkdir(d.File, 0o700)
	fatalMsg(t, d.Secure(os.Geteuid(), "/b"), "is not a regular file; refusing to use it.")
}

func TestSecureCreatesStagingAtMode700(t *testing.T) {
	d, _ := newDir(t)
	fi, err := os.Stat(d.Staging)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("staging: %v %v", fi, err)
	}
	os.Chmod(d.Staging, 0o750)
	fatalMsg(t, d.Secure(os.Geteuid(), "/b"), "is not mode 700; refusing to use it.")
}

func TestRefuseLegacy(t *testing.T) {
	home := t.TempDir()
	if err := RefuseLegacy(home, "/b"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(home, ".monad-failover"), 0o700)
	os.WriteFile(filepath.Join(home, ".monad-failover", "state"), []byte("last_step=3\n"), 0o600)
	fatalMsg(t, RefuseLegacy(home, "/b"), "Found state from an older version")
	// a dangling symlink counts too
	home2 := t.TempDir()
	os.MkdirAll(filepath.Join(home2, ".monad-failover", "promote"), 0o700)
	os.Symlink("/nonexistent", filepath.Join(home2, ".monad-failover", "promote", "state"))
	fatalMsg(t, RefuseLegacy(home2, "/b"), "Found state from an older version")
}

func TestLockRefusesSecondRun(t *testing.T) {
	d, _ := newDir(t)
	l1, err := d.AcquireLock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.AcquireLock()
	fatalMsg(t, err, "Another monad-failover run is already in progress on this host.")
	l1.Release()
	l2, err := d.AcquireLock()
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	l2.Release()
}

func TestCheckLastStepIsAClosedRange(t *testing.T) {
	d, _ := newDir(t)
	s := d.Store()
	for _, bad := range []string{"0", "9", "999", "a[$(touch /tmp/pwned)]", "$((1+1))", "3 ", "", "-1"} {
		if err := s.CheckLastStep(bad, "/b"); err == nil {
			t.Errorf("last_step %q accepted", bad)
		}
	}
	for _, good := range []string{"1", "5", "8"} {
		if err := s.CheckLastStep(good, "/b"); err != nil {
			t.Errorf("last_step %q refused: %v", good, err)
		}
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Fatal("injected command was executed")
	}
}

func writeState(t *testing.T, d Dir, content string) Store {
	t.Helper()
	if err := os.WriteFile(d.File, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return d.Store()
}

func TestValidateConsistency(t *testing.T) {
	sha := strings.Repeat("a", 64)
	cases := []struct {
		name, state, want string
	}{
		{"no file is fine", "", ""},
		{"plain progress", "last_step=3\nnetwork=testnet\n", ""},
		{"cutover at 7 with checksums", "last_step=7\ncutover_started=1\nstaged_secp_sha=" + sha + "\nstaged_bls_sha=" + sha + "\nstaged_toml_sha=" + sha + "\n", ""},
		{"cutover at 6 with checksums", "last_step=6\ncutover_started=1\nstaged_secp_sha=" + sha + "\nstaged_bls_sha=" + sha + "\nstaged_toml_sha=" + sha + "\n", ""},
		{"cutover flagged at step 5", "last_step=5\ncutover_started=1\nstaged_secp_sha=" + sha + "\nstaged_bls_sha=" + sha + "\nstaged_toml_sha=" + sha + "\n", "cutover is marked started but last_step is '5'"},
		{"cutover flagged, checksum missing", "last_step=7\ncutover_started=1\nstaged_secp_sha=" + sha + "\nstaged_bls_sha=" + sha + "\n", "a staged checksum is missing"},
		{"step 7 without the flag", "last_step=7\nstaged_secp_sha=" + sha + "\n", "reached step 7 but cutover is not marked started"},
		{"bad flag shape", "cutover_started=yes\n", "'cutover_started' holds an unexpected value"},
		{"bad checksum shape", "staged_secp_sha=ABC\n", "'staged_secp_sha' holds an unexpected value"},
		{"out-of-range step", "last_step=999\n", "'last_step' holds an unexpected value"},
	}
	for _, tc := range cases {
		d, _ := newDir(t)
		var s Store
		if tc.state == "" {
			s = d.Store()
		} else {
			s = writeState(t, d, tc.state)
		}
		err := s.ValidateConsistency("/opt/monad/backup")
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		fatalMsg(t, err, tc.want)
	}
}

func TestLoadResume(t *testing.T) {
	sha := strings.Repeat("b", 64)
	full := "last_step=6\nnetwork=testnet\nnew_seq=8\nsecp_pub=0xSECP1\nbls_pub=0xBLS2\nip=203.0.113.7\n" +
		"self_address=203.0.113.7:8000\nself_sig=abab\nself_seq=8\nself_auth_port=8001\n" +
		"beneficiary=0xBEEF00000000000000000000000000000000BEEF\nbackup_dir=/opt/monad/backup/failover-1\n" +
		"staged_secp_sha=" + sha + "\nstaged_bls_sha=" + sha + "\nstaged_toml_sha=" + sha + "\n"
	cases := []struct {
		name, state, want string
	}{
		{"complete step 6", full, ""},
		{"nothing to resume", "", "NO_PREVIOUS"},
		{"empty signed field at step 6", strings.Replace(full, "self_sig=abab\n", "self_sig=\n", 1), "'self_sig' is empty but the run had reached step 6"},
		{"missing network at step 2", "last_step=2\n", "'network' is empty but the run had reached step 2"},
		{"bogus network", strings.Replace(full, "network=testnet", "network=devnet", 1), "'network' holds an unexpected value"},
		{"malformed beneficiary", strings.Replace(full, "beneficiary=0xBEEF00000000000000000000000000000000BEEF", "beneficiary=0xZZ", 1), "'beneficiary' holds an unexpected value"},
		{"backup_dir outside root", strings.Replace(full, "backup_dir=/opt/monad/backup/failover-1", "backup_dir=/etc", 1), "'backup_dir' is not under /opt/monad/backup"},
		{"backup_dir traversal", strings.Replace(full, "backup_dir=/opt/monad/backup/failover-1", "backup_dir=/opt/monad/backup/../../etc", 1), "'backup_dir' is not under /opt/monad/backup"},
		{"ip out of range", strings.Replace(full, "ip=203.0.113.7", "ip=999.0.113.7", 1), "'ip' is not a valid IPv4 address"},
		{"ip wrong shape", strings.Replace(full, "ip=203.0.113.7", "ip=example.com", 1), "'ip' holds an unexpected value"},
		{"self_address shape", strings.Replace(full, "self_address=203.0.113.7:8000", "self_address=203.0.113.7", 1), "'self_address' holds an unexpected value"},
		{"seq not digits", strings.Replace(full, "new_seq=8", "new_seq=8a", 1), "'new_seq' holds an unexpected value"},
		{"pubkey with shell chars", strings.Replace(full, "secp_pub=0xSECP1", "secp_pub=$(id)", 1), "'secp_pub' holds an unexpected value"},
		{"last_step injection", strings.Replace(full, "last_step=6", "last_step=a[$(touch /tmp/pwned2)]", 1), "'last_step' holds an unexpected value"},
	}
	for _, tc := range cases {
		d, _ := newDir(t)
		var s Store
		if tc.state == "" {
			s = d.Store()
		} else {
			s = writeState(t, d, tc.state)
		}
		r, err := s.LoadResume("/opt/monad/backup", netinfo.ValidIPv4)
		switch tc.want {
		case "":
			if err != nil {
				t.Errorf("%s: unexpected %v", tc.name, err)
			} else if r.LastStep != 6 || r.SelfSeq != "8" || !r.CutoverStarted == true && r.StagedSecpSha != sha {
				t.Errorf("%s: fields %+v", tc.name, r)
			}
		case "NO_PREVIOUS":
			if !errors.Is(err, ErrNoPreviousRun) {
				t.Errorf("%s: got %v", tc.name, err)
			}
		default:
			if err == nil {
				t.Errorf("%s: accepted", tc.name)
				continue
			}
			fatalMsg(t, err, tc.want)
		}
	}
	if _, err := os.Stat("/tmp/pwned2"); err == nil {
		t.Fatal("injected command was executed")
	}
}
