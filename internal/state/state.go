// Package state keeps the resume record of a live run.
//
// A resume reads this record back and acts on it as root: it names the
// sequence to sign, the IP to publish and the directory a failed step points
// the operator at. Whoever can write it can steer the run, so it lives in a
// root-owned directory, every field is validated against a narrow shape
// before use, and nothing an unprivileged user could have staged (a symlink,
// a loose directory, a foreign owner) is accepted.
//
// The on-disk format is a plain key=value file.
package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// DefaultDir is where a live run keeps its state.
const DefaultDir = "/var/lib/monad-failover"

// Dir is the resolved state layout.
type Dir struct {
	Root    string
	File    string
	Staging string
	Lock    string

	SecpNew string
	BlsNew  string
	TomlNew string
}

// Resolve picks the state directory. The override is accepted only in the
// test sandbox; paths.FromEnv has already refused it elsewhere.
func Resolve(sandbox bool, override string) Dir {
	root := DefaultDir
	if sandbox && override != "" {
		root = override
	}
	d := Dir{Root: root}
	d.File = filepath.Join(root, "state")
	d.Staging = filepath.Join(root, "staging")
	d.Lock = filepath.Join(root, ".lock")
	d.SecpNew = filepath.Join(d.Staging, "id-secp.new")
	d.BlsNew = filepath.Join(d.Staging, "id-bls.new")
	d.TomlNew = filepath.Join(d.Staging, "node.toml.new")
	return d
}

// LegacyDir is a state location older releases used. It is under MONAD_HOME,
// which the monad service account can write, so it is refused, never read.
func LegacyDir(monadHome string) string { return filepath.Join(monadHome, ".monad-failover") }

// RefuseLegacy stops the run if an old-layout state file exists.
func RefuseLegacy(monadHome, backupRoot string) error {
	legacy := LegacyDir(monadHome)
	for _, f := range []string{filepath.Join(legacy, "state"), filepath.Join(legacy, "promote", "state")} {
		if _, err := os.Lstat(f); err == nil {
			return ui.Die(
				"Found state from an older version at "+f+".",
				"That path is under "+monadHome+" and writable by the monad service",
				"account, so it is not trusted and is not migrated automatically.",
				"Review it, then remove the directory:  rm -rf "+legacy,
				"If a previous run was interrupted, restore this node from "+backupRoot,
				"and start a fresh run rather than resuming from it.")
		}
	}
	return nil
}

func modeBits(fi fs.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint32(st.Mode) & 0o7777
	}
	return uint32(fi.Mode().Perm())
}

func ownerUID(fi fs.FileInfo) (uint32, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid, true
	}
	return 0, false
}

// Secure creates the state and staging directories at 0700 when absent and
// refuses anything that is not exactly what a previous run of this tool
// would have left: a symlink standing in for a directory or the file, a
// directory whose mode is not 0700, or (as root) a foreign owner. An
// existing directory is never repaired and then trusted.
func (d Dir) Secure(euid int, backupRoot string) error {
	if fi, err := os.Lstat(d.Root); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return ui.Die(d.Root+" is a symlink; refusing to use it.",
			"Remove it and re-run so the directory is created directly.")
	}
	if _, err := os.Stat(d.Root); err != nil {
		if err := os.MkdirAll(d.Root, 0o700); err != nil {
			return ui.Die("Could not create " + d.Root)
		}
	}
	fi, err := os.Stat(d.Root)
	if err != nil {
		return ui.Die("Could not create " + d.Root)
	}
	if m := modeBits(fi); m != 0o700 {
		return ui.Die(fmt.Sprintf("%s has mode %o, not 700; refusing to use it.", d.Root, m),
			"It may have been writable by another user, so its contents are not",
			"trusted. Remove it and re-run:  rm -rf "+d.Root)
	}
	if euid == 0 {
		if uid, ok := ownerUID(fi); ok && uid != 0 {
			return ui.Die(fmt.Sprintf("%s is owned by uid %d, not root; refusing to use it.", d.Root, uid),
				"Resume state must not be writable by the monad service account.",
				"Remove it and re-run:  rm -rf "+d.Root)
		}
	}

	if fi, err := os.Lstat(d.File); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return ui.Die(d.File+" is a symlink; refusing to read or write through it.",
			"Remove it. If a run was interrupted, restore this node from",
			backupRoot+" and start a fresh run.")
	}
	if fi, err := os.Lstat(d.Staging); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return ui.Die(d.Staging+" is a symlink; refusing to use it.", "Remove it and re-run.")
	}
	if _, err := os.Stat(d.Staging); err != nil {
		if err := os.MkdirAll(d.Staging, 0o700); err != nil {
			return ui.Die("Could not create " + d.Staging)
		}
	}
	if fi, err := os.Stat(d.Staging); err != nil || modeBits(fi) != 0o700 {
		return ui.Die(d.Staging + " is not mode 700; refusing to use it. Remove it and re-run.")
	}

	if fi, err := os.Lstat(d.File); err == nil {
		if !fi.Mode().IsRegular() {
			return ui.Die(d.File + " is not a regular file; refusing to use it.")
		}
		if euid == 0 {
			if uid, ok := ownerUID(fi); ok && uid != 0 {
				return ui.Die(fmt.Sprintf("%s is owned by uid %d, not root; refusing to use it.", d.File, uid),
					"Remove it and start a fresh run.")
			}
		}
		// Narrow only, never widen: defence in depth behind the 0700 directory.
		_ = os.Chmod(d.File, 0o600)
	}
	return nil
}

// RunLock is the exclusive lock held for the life of the process. Keep the
// returned value reachable: the descriptor is released when it is closed.
type RunLock struct{ f *os.File }

// AcquireLock takes the run lock without blocking. A second run is refused
// before it has read or written anything.
func (d Dir) AcquireLock() (*RunLock, error) {
	f, err := os.OpenFile(d.Lock, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, ui.Die("Could not open the run lock at " + d.Lock)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ui.Die("Another monad-failover run is already in progress on this host.",
			"Nothing has been read or changed by this invocation.",
			"Wait for it to finish, or check for a stuck run:  fuser -v "+d.Lock)
	}
	return &RunLock{f: f}, nil
}

// Release drops the lock. Process exit does the same.
func (l *RunLock) Release() {
	if l != nil && l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// Store reads and writes the key=value record.
type Store struct {
	dir Dir
}

func (d Dir) Store() Store { return Store{dir: d} }

// Exists reports whether a state file is present.
func (s Store) Exists() bool {
	fi, err := os.Lstat(s.dir.File)
	return err == nil && fi.Mode().IsRegular()
}

func (s Store) lines() []string {
	b, err := os.ReadFile(s.dir.File)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// Get returns the value stored under key, "" when absent. A duplicated key
// yields every value joined by newlines, which no field shape accepts, so a
// tampered file with repeated keys is refused by validation rather than
// silently read as its first line.
func (s Store) Get(key string) string {
	var vals []string
	for _, l := range s.lines() {
		if strings.HasPrefix(l, key+"=") {
			vals = append(vals, strings.TrimPrefix(l, key+"="))
		}
	}
	return strings.Join(vals, "\n")
}

// Set records key=value atomically: the file is rewritten to a temporary
// name created inside the root-only directory (never a predictable path)
// and renamed over the old one.
func (s Store) Set(key, value string) error {
	var out []string
	for _, l := range s.lines() {
		if l == "" || strings.HasPrefix(l, key+"=") {
			continue
		}
		out = append(out, l)
	}
	out = append(out, key+"="+value)
	tmp, err := os.CreateTemp(s.dir.Root, ".state.*")
	if err != nil {
		return ui.Die("Could not write to " + s.dir.Root)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(strings.Join(out, "\n") + "\n"); err != nil {
		tmp.Close()
		os.Remove(name)
		return ui.Die("Could not write to " + s.dir.Root)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return ui.Die("Could not write to " + s.dir.Root)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return ui.Die("Could not write to " + s.dir.Root)
	}
	_ = os.Chmod(name, 0o600)
	if err := os.Rename(name, s.dir.File); err != nil {
		os.Remove(name)
		return ui.Die("Could not write to " + s.dir.Root)
	}
	return nil
}

// Clear removes the state file.
func (s Store) Clear() { _ = os.Remove(s.dir.File) }

// CompletedStep reports whether the recorded last_step is a valid step at or
// past n.
func (s Store) CompletedStep(n int) bool {
	v := s.Get("last_step")
	if !stepRe.MatchString(v) {
		return false
	}
	c, _ := strconv.Atoi(v)
	return c >= n
}

// ── field validation ────────────────────────────────────────────────

var (
	stepRe    = regexp.MustCompile(`^[1-8]$`)
	sha64Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	optSha64  = regexp.MustCompile(`^([0-9a-f]{64})?$`)
	flagRe    = regexp.MustCompile(`^1?$`)
	networkRe = regexp.MustCompile(`^(mainnet|testnet)?$`)
	digitsRe  = regexp.MustCompile(`^[0-9]*$`)
	alnumRe   = regexp.MustCompile(`^[0-9A-Za-z]*$`)
	addrRe    = regexp.MustCompile(`^([0-9.]+:[0-9]+)?$`)
	portOptRe = regexp.MustCompile(`^[0-9]{0,5}$`)
	benRe     = regexp.MustCompile(`^(0x[0-9A-Fa-f]{40})?$`)
	ipOptRe   = regexp.MustCompile(`^([0-9]{1,3}(\.[0-9]{1,3}){3})?$`)
	pathRe    = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
)

// CheckField refuses a value whose shape is wrong. Every field reaches a
// config file, a signed record, a path or a command line, so the shapes are
// deliberately narrow.
func (s Store) CheckField(key, val string, re *regexp.Regexp, backupRoot string) error {
	if re.MatchString(val) {
		return nil
	}
	return ui.Die("State file is corrupt or was tampered with: '"+key+"' holds an unexpected value.",
		"Refusing to resume from it.",
		"Restore this node from "+backupRoot+" if a run was interrupted, remove",
		s.dir.File+", then start a fresh run.")
}

// CheckLastStep validates last_step's shape (a closed 1..8 range, not just
// digits: an out-of-range value would satisfy every completed-step check and
// skip the whole migration to a fake completion).
func (s Store) CheckLastStep(val, backupRoot string) error {
	return s.CheckField("last_step", val, stepRe, backupRoot)
}

// ValidateConsistency enforces the cross-field rules before both the resume
// path and the fresh-run decision, so a tampered or truncated file slips past
// neither.
func (s Store) ValidateConsistency(backupRoot string) error {
	if !s.Exists() {
		return nil
	}
	ls := s.Get("last_step")
	cs := s.Get("cutover_started")
	s1, s2, s3 := s.Get("staged_secp_sha"), s.Get("staged_bls_sha"), s.Get("staged_toml_sha")

	if err := s.CheckField("cutover_started", cs, flagRe, backupRoot); err != nil {
		return err
	}
	for _, kv := range []struct{ k, v string }{{"staged_secp_sha", s1}, {"staged_bls_sha", s2}, {"staged_toml_sha", s3}} {
		if err := s.CheckField(kv.k, kv.v, optSha64, backupRoot); err != nil {
			return err
		}
	}
	if ls != "" {
		if err := s.CheckField("last_step", ls, stepRe, backupRoot); err != nil {
			return err
		}
	}

	if cs == "1" {
		if ls != "6" && ls != "7" && ls != "8" {
			return ui.Die("State file is inconsistent: cutover is marked started but last_step is '"+ls+"'.",
				"Refusing to act on it.",
				"Restore this node from "+backupRoot+", remove "+s.dir.File+", then start fresh.")
		}
		if s1 == "" || s2 == "" || s3 == "" {
			return ui.Die("State file is inconsistent: cutover is marked started but a staged checksum is missing.",
				"Refusing to act on it.",
				"Restore this node from "+backupRoot+", remove "+s.dir.File+", then start fresh.")
		}
	}
	if ls != "" {
		n, _ := strconv.Atoi(ls)
		if n >= 7 && cs != "1" {
			return ui.Die("State file is inconsistent: it reached step "+ls+" but cutover is not marked started.",
				"Refusing to act on it.",
				"Finish with --resume only if the cutover truly completed; otherwise restore",
				"from "+backupRoot+", remove "+s.dir.File+", and start fresh.")
		}
	}
	return nil
}

// Resume is the validated content of a state file at resume time.
type Resume struct {
	LastStep       int
	Network        string
	NewSeq         string
	SecpPub        string
	BlsPub         string
	IP             string
	SelfAddress    string
	SelfSig        string
	SelfSeq        string
	SelfAuthPort   string
	Beneficiary    string
	BackupDir      string
	CutoverStarted bool
	StagedSecpSha  string
	StagedBlsSha   string
	StagedTomlSha  string
}

// ErrNoPreviousRun is returned by LoadResume when there is nothing to resume.
var ErrNoPreviousRun = errors.New("no previous run")

// LoadResume reads and validates every field a resume acts on. Shapes are the
// real format of each field, not a loose charset; presence is step-dependent,
// because a blank field past the step that writes it means the file was
// truncated or tampered with.
func (s Store) LoadResume(backupRoot string, validIPv4 func(string) bool) (Resume, error) {
	var r Resume
	last := s.Get("last_step")
	if last == "" {
		return r, ErrNoPreviousRun
	}
	if err := s.CheckLastStep(last, backupRoot); err != nil {
		return r, err
	}
	r.LastStep, _ = strconv.Atoi(last)

	r.Network = s.Get("network")
	r.NewSeq = s.Get("new_seq")
	r.SecpPub = s.Get("secp_pub")
	r.BlsPub = s.Get("bls_pub")
	r.IP = s.Get("ip")
	r.SelfAddress = s.Get("self_address")
	r.SelfSig = s.Get("self_sig")
	r.SelfSeq = s.Get("self_seq")
	r.SelfAuthPort = s.Get("self_auth_port")
	r.Beneficiary = s.Get("beneficiary")
	r.BackupDir = s.Get("backup_dir")
	cs := s.Get("cutover_started")
	r.StagedSecpSha = s.Get("staged_secp_sha")
	r.StagedBlsSha = s.Get("staged_bls_sha")
	r.StagedTomlSha = s.Get("staged_toml_sha")

	checks := []struct {
		k, v string
		re   *regexp.Regexp
	}{
		{"network", r.Network, networkRe},
		{"new_seq", r.NewSeq, digitsRe},
		{"self_seq", r.SelfSeq, digitsRe},
		{"secp_pub", r.SecpPub, alnumRe},
		{"bls_pub", r.BlsPub, alnumRe},
		{"self_sig", r.SelfSig, alnumRe},
		{"self_address", r.SelfAddress, addrRe},
		{"self_auth_port", r.SelfAuthPort, portOptRe},
		{"beneficiary", r.Beneficiary, benRe},
		{"cutover_started", cs, flagRe},
		{"staged_secp_sha", r.StagedSecpSha, optSha64},
		{"staged_bls_sha", r.StagedBlsSha, optSha64},
		{"staged_toml_sha", r.StagedTomlSha, optSha64},
		{"ip", r.IP, ipOptRe},
		{"backup_dir", r.BackupDir, pathRe},
	}
	for _, c := range checks {
		if err := s.CheckField(c.k, c.v, c.re, backupRoot); err != nil {
			return r, err
		}
	}
	r.CutoverStarted = cs == "1"
	if r.IP != "" && !validIPv4(r.IP) {
		return r, ui.Die("State file is corrupt or was tampered with: 'ip' is not a valid IPv4 address.",
			"Refusing to resume from it.",
			"Remove "+s.dir.File+" and start a fresh run.")
	}
	// backup_dir is only ever shown to the operator as the place to restore
	// from; a tampered value would send them to an attacker-chosen path.
	if r.BackupDir != "" {
		if strings.Contains(r.BackupDir, "..") || !strings.HasPrefix(r.BackupDir, backupRoot+"/") {
			return r, ui.Die("State file is corrupt or was tampered with: 'backup_dir' is not under "+backupRoot+".",
				"Refusing to resume: recovery messages would point at that path.",
				"Remove "+s.dir.File+" and start a fresh run.")
		}
	}

	require := func(minStep int, name, val string) error {
		if r.LastStep >= minStep && val == "" {
			return ui.Die("State file is incomplete: '"+name+"' is empty but the run had reached step "+last+".",
				"Refusing to resume from a partial state file.",
				"Restore this node from "+backupRoot+" if a run was interrupted, remove",
				s.dir.File+", then start a fresh run.")
		}
		return nil
	}
	for _, rq := range []struct {
		step int
		name string
		val  string
	}{
		{2, "network", r.Network},
		{4, "secp_pub", r.SecpPub},
		{4, "bls_pub", r.BlsPub},
		{5, "new_seq", r.NewSeq},
		{6, "ip", r.IP},
		{6, "self_address", r.SelfAddress},
		{6, "self_sig", r.SelfSig},
		{6, "self_seq", r.SelfSeq},
		{6, "self_auth_port", r.SelfAuthPort},
	} {
		if err := require(rq.step, rq.name, rq.val); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Sha64 reports whether v is a lowercase hex SHA-256.
func Sha64(v string) bool { return sha64Re.MatchString(v) }
