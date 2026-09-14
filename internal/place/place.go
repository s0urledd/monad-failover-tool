// Package place moves confirmed staging files into the live config directory
// without ever leaving an unverified or half-written file behind.
package place

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/s0urledd/monad-failover-tool/internal/state"
	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// Rename is the syscall used for the final step; tests swap it to simulate a
// rename failing mid-cutover.
var Rename = os.Rename

// FileSHA returns the lowercase hex SHA-256 of path, "" when unreadable.
func FileSHA(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func isSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

func isRegular(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// VerifyStagedOrPlaced allows a staged file to be placed only if it still
// matches the checksum recorded when it was created and confirmed, or if an
// earlier cutover attempt already moved exactly that content into place. The
// recorded checksum is never refreshed from disk: re-hashing here would
// launder a file modified after confirmation into the expected value.
func VerifyStagedOrPlaced(staged, live, want, label string) error {
	if !state.Sha64(want) {
		return ui.Die("No recorded checksum for "+label+" — refusing to place it.",
			"Start a fresh run so the staging steps re-create and record it.")
	}
	if isSymlink(staged) {
		return ui.Die(staged + " is a symlink — refusing to place it.")
	}
	if _, err := os.Lstat(staged); err == nil {
		if !isRegular(staged) {
			return ui.Die(staged + " is not a regular file — refusing to place it.")
		}
		if FileSHA(staged) != want {
			return ui.Die(label+" changed after it was prepared and confirmed.",
				"Refusing to swap it in. Nothing has been changed on the live node.",
				"Start a fresh run so the staging steps re-create it.")
		}
		return nil
	}
	// Gone from staging: it must already be in place from an earlier attempt.
	if isSymlink(live) {
		return ui.Die(live + " is a symlink — refusing to continue.")
	}
	if !isRegular(live) || FileSHA(live) != want {
		return ui.Die("Staging files are missing — refusing to stop services.",
			label+" is neither staged nor already in place with the confirmed content.",
			"Start a fresh run so the import and configure steps re-create them.")
	}
	return nil
}

// ErrNotPlaced is returned when the copy or rename failed and nothing was
// changed; the caller adds the recovery instructions for its own stage.
var ErrNotPlaced = errors.New("could not place file")

// Verified copies a staged file into the live location.
//
// Staging is on the root-owned state filesystem, which is not necessarily
// the one holding the live config, and a rename across filesystems is a copy
// rather than an atomic operation. So the content is written into the
// destination directory first, re-checked there, and only then renamed: a
// rename within one directory, which is atomic. The destination directory
// belongs to the monad account, so the temporary file is created and
// written in ONE open with O_CREAT|O_EXCL at mode 0600. Creating it and
// then reopening it by name would leave a window in which the name could
// be replaced with a symlink; with O_EXCL the open simply fails if anything
// is already there. The live file is verified once more after the rename.
//
// It returns (true, nil) when the file was already in place from an earlier
// attempt, (false, nil) when it was placed now, ErrNotPlaced when nothing
// changed, and a Fatal when the node must not be started.
func Verified(staged, live, want, label, restoreFrom string) (already bool, err error) {
	if _, statErr := os.Lstat(staged); statErr != nil {
		if isSymlink(live) {
			return false, ui.Die(live + " is a symlink — refusing to continue.")
		}
		if !isRegular(live) || FileSHA(live) != want {
			return false, ui.Die(label + " is neither staged nor already in place with the confirmed content.")
		}
		return true, nil
	}

	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := fmt.Sprintf("%s.mf-%d-%s", live, os.Getpid(), hex.EncodeToString(rnd[:]))

	src, err := os.Open(staged)
	if err != nil {
		return false, ErrNotPlaced
	}
	defer src.Close()
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, ErrNotPlaced
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmp)
		return false, ErrNotPlaced
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(tmp)
		return false, ErrNotPlaced
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return false, ErrNotPlaced
	}
	if FileSHA(tmp) != want {
		os.Remove(tmp)
		return false, ui.Die(label+" did not copy intact — refusing to place it.",
			"Nothing has been swapped. Start a fresh run.")
	}
	if err := Rename(tmp, live); err != nil {
		os.Remove(tmp)
		return false, ErrNotPlaced
	}
	if FileSHA(live) != want {
		return false, ui.Die(label+" does not match its confirmed checksum after placement.",
			"Do NOT start the services. Restore this node from "+restoreFrom+".")
	}
	_ = os.Remove(staged)
	return false, nil
}

// CheckLive verifies one live file still matches what the cutover placed.
func CheckLive(path, want, label, restoreFrom string) error {
	if !state.Sha64(want) {
		return ui.Die("No recorded checksum for "+label+"; refusing to start the services.",
			"Restore this node from "+restoreFrom+" and start a fresh run.")
	}
	if isSymlink(path) {
		return ui.Die(path + " is a symlink; refusing to start the services.")
	}
	if !isRegular(path) || FileSHA(path) != want {
		return ui.Die(label+" no longer matches what the cutover placed.",
			"Refusing to start the services with an identity that changed since.",
			"Restore this node from "+restoreFrom+" and start a fresh run.")
	}
	return nil
}

// CopyPreserve copies src to dst keeping mode and timestamps, and ownership
// when the process may set it (as root). Used for the identity backup.
func CopyPreserve(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = os.Chmod(dst, fi.Mode().Perm())
	_ = os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	if st, ok := fi.Sys().(*syscallStat); ok {
		_ = os.Chown(dst, int(st.Uid), int(st.Gid))
	}
	return nil
}
