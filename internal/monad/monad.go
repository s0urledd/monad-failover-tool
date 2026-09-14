// Package monad wraps the Monad command-line tools the migration depends on:
// monad-keystore, monad-sign-name-record and monad-status. Commands are
// executed with argument vectors, never through a shell.
//
// The key tools take the keystore password and IKM as command-line flags,
// so those values are visible in /proc/<pid>/cmdline while a child runs.
// That is a property of the Monad CLI. What this package controls is that
// no output of a secret-bearing call reaches the run log: stdout is parsed
// or discarded and stderr is dropped, so an error path that echoed its
// arguments cannot persist them.
package monad

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// scrub runs after every command that carried a secret on its argument
// vector. The strings built for exec are unreachable by then; a collection
// plus a return of free memory to the kernel takes them out of this
// process's address space where the allocator allows. It is a narrowing,
// not a guarantee: SECURITY.md says so.
func scrub() {
	runtime.GC()
	debug.FreeOSMemory()
}

// Have reports whether cmd is on PATH.
func Have(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// Tools carries the keystore password for the key commands. The password
// is held as bytes so the caller can zero it when the run ends.
type Tools struct {
	Password []byte
	EnvFile  string // named in error messages
}

// ImportKey creates a keystore at path from the IKM. All output is discarded.
func (t Tools) ImportKey(ikm []byte, path string) error {
	cmd := exec.Command("monad-keystore", "import",
		"--ikm", string(ikm),
		"--keystore-path", path,
		"--password", string(t.Password))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	err := cmd.Run()
	scrub()
	if err != nil {
		return ui.Die("monad-keystore import failed for "+path+".",
			"(Its output is suppressed so secrets can never reach the run log.)",
			"Check the keystore password in "+t.EnvFile+" and the IKM source, then re-run.")
	}
	if uid, gid, ok := monadIDs(); ok {
		_ = os.Chown(path, uid, gid)
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// RecoverPubkey returns the public key printed by `monad-keystore recover`
// for the keystore at path; "" when the tool fails or the line is absent.
func (t Tools) RecoverPubkey(path, keyType string) string {
	label := "BLS public key"
	if keyType == "secp" {
		label = "Secp public key"
	}
	cmd := exec.Command("monad-keystore", "recover",
		"--password", string(t.Password),
		"--keystore-path", path,
		"--key-type", keyType)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	_ = cmd.Run()
	// The output also carries the secret ("Keystore secret: ..."). Only the
	// public key line is copied out; the buffer is zeroed before returning.
	defer scrub()
	defer ui.Zero(out.Bytes())
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		if !containsFold(line, []byte(label)) {
			continue
		}
		f := bytes.Fields(line)
		if len(f) == 0 {
			return ""
		}
		return string(f[len(f)-1])
	}
	return ""
}

// containsFold is a case-insensitive substring test that allocates nothing,
// so no lowercase copy of a line holding a secret is ever made.
func containsFold(line, label []byte) bool {
	for i := 0; i+len(label) <= len(line); i++ {
		if bytes.EqualFold(line[i:i+len(label)], label) {
			return true
		}
	}
	return false
}

// ExportBackup writes the official-format backup of the keystore at path to
// out, atomically: the tool's output goes to out+".partial" and is renamed
// into place only on success, so a failure never leaves a truncated file.
func (t Tools) ExportBackup(path, keyType, out string) error {
	partial := out + ".partial"
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command("monad-keystore", "recover",
		"--password", string(t.Password),
		"--keystore-path", path,
		"--key-type", keyType)
	cmd.Stdout, cmd.Stderr = f, io.Discard
	runErr := cmd.Run()
	scrub()
	closeErr := f.Close()
	if runErr != nil || closeErr != nil {
		os.Remove(partial)
		if runErr != nil {
			return runErr
		}
		return closeErr
	}
	if err := os.Rename(partial, out); err != nil {
		os.Remove(partial)
		return err
	}
	_ = os.Chmod(out, 0o600)
	return nil
}

// SignNameRecord runs the signer with the standard P2P ports and returns its
// stdout. stderr is dropped: the call carries the password on argv.
func (t Tools) SignNameRecord(ip, seq, keystorePath string) (string, error) {
	// No --node-config on purpose: with it the signer reads the seq from the
	// current node.toml (stale on a fresh full node) and ignores the argument.
	cmd := exec.Command("monad-sign-name-record",
		"--ip", ip,
		"--tcp-port", "8000",
		"--udp-port", "8000",
		"--authenticated-udp-port", "8001",
		"--self-record-seq-num", seq,
		"--keystore-path", keystorePath,
		"--password", string(t.Password))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	err := cmd.Run()
	scrub()
	if err != nil {
		return "", ui.Die("monad-sign-name-record failed.",
			"(Its error output is suppressed so secrets can never reach the run log.)",
			"Check the keystore password in "+t.EnvFile+" and the monad version, then re-run.")
	}
	return out.String(), nil
}

// Status runs monad-status and returns the status and blockDifference
// fields. ok is false when the tool is missing or printed no status.
func Status() (status, blockDiff string, ok bool) {
	cmd := exec.Command("monad-status")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	_ = cmd.Run()
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch {
		case f[0] == "status:" && status == "":
			status = f[1]
		case f[0] == "blockDifference:" && blockDiff == "":
			blockDiff = f[1]
		}
	}
	return status, blockDiff, status != ""
}

// Clear clears the terminal before a live run; failure is ignored.
func Clear(w io.Writer) {
	cmd := exec.Command("clear")
	cmd.Stdout = w
	_ = cmd.Run()
}

var errNoMonadUser = errors.New("monad user not found")

// MonadIDs returns the uid and gid of the monad service account.
func MonadIDs() (int, int, error) {
	uid, gid, ok := monadIDs()
	if !ok {
		return 0, 0, errNoMonadUser
	}
	return uid, gid, nil
}
