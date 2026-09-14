package place

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

func setup(t *testing.T, content string) (staged, live, want string) {
	t.Helper()
	dir := t.TempDir()
	staged = filepath.Join(dir, "staging", "id-secp.new")
	live = filepath.Join(dir, "config", "id-secp")
	os.MkdirAll(filepath.Dir(staged), 0o700)
	os.MkdirAll(filepath.Dir(live), 0o755)
	os.WriteFile(staged, []byte(content), 0o600)
	os.WriteFile(live, []byte("old identity"), 0o600)
	return staged, live, FileSHA(staged)
}

func fatalMsg(t *testing.T, err error, want string) {
	t.Helper()
	var f *ui.Fatal
	if !errors.As(err, &f) || !strings.Contains(f.Msg+"\n"+strings.Join(f.Lines, "\n"), want) {
		t.Fatalf("expected Fatal containing %q, got %v", want, err)
	}
}

func TestFileSHA(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("abc"), 0o600)
	if got := FileSHA(p); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("sha = %s", got)
	}
	if got := FileSHA("/nonexistent"); got != "" {
		t.Errorf("missing file sha = %q", got)
	}
}

func TestVerifiedPlacesAndCleansUp(t *testing.T) {
	staged, live, want := setup(t, "new identity")
	already, err := Verified(staged, live, want, "SECP key", "/b")
	if err != nil || already {
		t.Fatalf("place: already=%v err=%v", already, err)
	}
	got, _ := os.ReadFile(live)
	if string(got) != "new identity" {
		t.Errorf("live = %q", got)
	}
	if _, err := os.Stat(staged); err == nil {
		t.Error("staged file left behind")
	}
	fi, _ := os.Stat(live)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("live mode %v", fi.Mode())
	}
	entries, _ := os.ReadDir(filepath.Dir(live))
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
	// second attempt: already in place
	already, err = Verified(staged, live, want, "SECP key", "/b")
	if err != nil || !already {
		t.Errorf("resume: already=%v err=%v", already, err)
	}
}

func TestVerifiedRenameFailureLeavesNothingChanged(t *testing.T) {
	staged, live, want := setup(t, "new identity")
	old := Rename
	Rename = func(a, b string) error { return errors.New("simulated") }
	defer func() { Rename = old }()
	_, err := Verified(staged, live, want, "SECP key", "/b")
	if !errors.Is(err, ErrNotPlaced) {
		t.Fatalf("got %v", err)
	}
	got, _ := os.ReadFile(live)
	if string(got) != "old identity" {
		t.Error("live file changed")
	}
	if _, err := os.Stat(staged); err != nil {
		t.Error("staged file removed although nothing was placed")
	}
	entries, _ := os.ReadDir(filepath.Dir(live))
	if len(entries) != 1 {
		t.Errorf("temporary file left behind: %v", entries)
	}
}

func TestVerifiedRefusesWhenNeitherStagedNorPlaced(t *testing.T) {
	staged, live, want := setup(t, "new identity")
	os.Remove(staged)
	_, err := Verified(staged, live, want, "SECP key", "/b")
	fatalMsg(t, err, "neither staged nor already in place")
	os.Remove(live)
	os.Symlink("/etc/hostname", live)
	_, err = Verified(staged, live, want, "SECP key", "/b")
	fatalMsg(t, err, "is a symlink — refusing to continue.")
}

func TestVerifyStagedOrPlaced(t *testing.T) {
	staged, live, want := setup(t, "new identity")
	if err := VerifyStagedOrPlaced(staged, live, want, "the SECP key"); err != nil {
		t.Fatal(err)
	}
	// changed after confirmation
	os.WriteFile(staged, []byte("tampered"), 0o600)
	fatalMsg(t, VerifyStagedOrPlaced(staged, live, want, "the SECP key"), "changed after it was prepared and confirmed.")
	// missing recorded checksum
	fatalMsg(t, VerifyStagedOrPlaced(staged, live, "", "the SECP key"), "No recorded checksum for the SECP key")
	fatalMsg(t, VerifyStagedOrPlaced(staged, live, "ABC", "the SECP key"), "No recorded checksum for the SECP key")
	// symlink in staging
	os.Remove(staged)
	os.Symlink(live, staged)
	fatalMsg(t, VerifyStagedOrPlaced(staged, live, want, "the SECP key"), "is a symlink — refusing to place it.")
	// gone from staging, live has the confirmed content: fine
	os.Remove(staged)
	os.WriteFile(live, []byte("new identity"), 0o600)
	if err := VerifyStagedOrPlaced(staged, live, want, "the SECP key"); err != nil {
		t.Errorf("already placed: %v", err)
	}
	// gone from staging, live differs: refuse before stopping anything
	os.WriteFile(live, []byte("other"), 0o600)
	fatalMsg(t, VerifyStagedOrPlaced(staged, live, want, "the SECP key"), "Staging files are missing — refusing to stop services.")
}

func TestCheckLive(t *testing.T) {
	_, live, _ := setup(t, "x")
	want := FileSHA(live)
	if err := CheckLive(live, want, "the SECP key", "/b"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(live, []byte("changed"), 0o600)
	fatalMsg(t, CheckLive(live, want, "the SECP key", "/b"), "no longer matches what the cutover placed.")
	fatalMsg(t, CheckLive(live, "", "the SECP key", "/b"), "No recorded checksum for the SECP key; refusing to start the services.")
	os.Remove(live)
	os.Symlink("/etc/hostname", live)
	fatalMsg(t, CheckLive(live, want, "the SECP key", "/b"), "is a symlink; refusing to start the services.")
}

func TestCopyPreserve(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.WriteFile(src, []byte("content"), 0o640)
	dst := filepath.Join(dir, "dst")
	if err := CopyPreserve(src, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	fi, _ := os.Stat(dst)
	if string(got) != "content" || fi.Mode().Perm() != 0o640 {
		t.Errorf("copy: %q %v", got, fi.Mode())
	}
}
