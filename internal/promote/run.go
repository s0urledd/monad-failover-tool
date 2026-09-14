// Package promote is the migration itself: eight phases, every one of them
// resumable, with every irreversible action behind an explicit confirmation.
package promote

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/s0urledd/monad-failover-tool/internal/foundation"
	"github.com/s0urledd/monad-failover-tool/internal/monad"
	"github.com/s0urledd/monad-failover-tool/internal/netinfo"
	"github.com/s0urledd/monad-failover-tool/internal/nodeconf"
	"github.com/s0urledd/monad-failover-tool/internal/paths"
	"github.com/s0urledd/monad-failover-tool/internal/place"
	"github.com/s0urledd/monad-failover-tool/internal/state"
	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// PhasesTotal is the number of banners a live run prints.
const PhasesTotal = 8

// Options are the command-line choices for a live run.
type Options struct {
	Resume       bool
	KeySourceDir string // --backup-dir; "" asks, "-" means manual IKM entry
	PublicIP     string // --public-ip override
	Version      string
	Argv0        string // how the tool was invoked, for "re-run with" hints
}

// Run holds everything one live run knows.
type Run struct {
	c   *ui.Console
	p   paths.Paths
	d   state.Dir
	st  state.Store
	opt Options

	tools monad.Tools

	network      string
	newSeq       string
	secpPub      string
	blsPub       string
	ip           string
	selfAddress  string
	selfSig      string
	selfSeq      string
	selfAuthPort string
	beneficiary  string
	backupDir    string

	foundSeq      uint64
	foundKnown    bool
	verifyPending bool
	cachedIP      string

	// injectable clocks for the test suite
	sleep func(time.Duration)
	now   func() time.Time
}

// New builds a run. The caller has already resolved paths and state.
func New(c *ui.Console, p paths.Paths, d state.Dir, opt Options) *Run {
	return &Run{
		c: c, p: p, d: d, st: d.Store(), opt: opt,
		sleep: time.Sleep, now: time.Now,
	}
}

// Prepare performs the checks every live run makes before touching state:
// the root gate, the refusal of old-layout state, the state directory
// checks, the run lock, the consistency rules, and the fresh-or-resume
// decision. It returns (false, nil) when the operator chose to stop.
func (r *Run) Prepare() (proceed bool, lock *state.RunLock, err error) {
	if r.p.EUID != 0 && !r.p.Sandbox {
		return false, nil, ui.Die("monad-failover must run as root.")
	}
	if err := state.RefuseLegacy(r.p.MonadHome, r.p.BackupRoot); err != nil {
		return false, nil, err
	}
	if err := r.d.Secure(r.p.EUID, r.p.BackupRoot); err != nil {
		return false, nil, err
	}
	// Exclusive for the whole live run, taken before any state is read or
	// written, so a second process is refused without touching anything.
	lock, err = r.d.AcquireLock()
	if err != nil {
		return false, nil, err
	}
	if err := r.st.ValidateConsistency(r.p.BackupRoot); err != nil {
		return false, lock, err
	}

	if !r.opt.Resume && r.st.Exists() {
		last := r.st.Get("last_step")
		if last != "" {
			if err := r.st.CheckLastStep(last, r.p.BackupRoot); err != nil {
				return false, lock, err
			}
			// Once a cutover has begun, "start fresh" is no longer safe: it
			// would re-snapshot a possibly half-swapped identity as this
			// node's own. The interrupted run must be finished with --resume.
			if r.st.Get("cutover_started") == "1" {
				return false, lock, ui.Die("A previous run reached cutover — starting fresh is not safe now.",
					"Finish the interrupted run instead: "+r.opt.Argv0+" --resume",
					"(Only if you have manually restored this node and are sure, delete",
					r.d.File+" to allow a fresh run.)")
			}
			r.c.Warn("Previous run stopped at step " + last)
			r.c.Println("  Run with --resume to continue, or start fresh.")
			if r.c.ConfirmYN("  Start fresh?") {
				r.st.Clear()
			} else {
				r.c.Println("  Use: " + r.opt.Argv0 + " --resume")
				return false, lock, nil
			}
		}
	}
	return true, lock, nil
}

func (r *Run) startLog() error {
	if err := os.MkdirAll(r.p.LogDir, 0o700); err != nil {
		return ui.Die("Could not create the log directory " + r.p.LogDir)
	}
	_ = os.Chmod(r.p.LogDir, 0o700)
	logPath := filepath.Join(r.p.LogDir, "failover-"+r.now().Format("20060102-150405")+".log")
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return ui.Die("Could not open the run log " + logPath)
	}
	// Secrets never appear in the output (hidden input, no echo), so the log
	// is safe to keep; it is the operator's record of the migration.
	r.c.Tee(f)
	r.c.Printf("%s(logging this run to %s)%s\n", ui.Dim, logPath, ui.Reset)
	return nil
}

// Promote runs the migration from wherever the state says it stands.
func (r *Run) Promote() error {
	if err := r.startLog(); err != nil {
		return err
	}
	r.c.Header(r.opt.Version)

	for _, cmd := range []string{"systemctl", "monad-keystore", "monad-sign-name-record"} {
		if !monad.Have(cmd) {
			return ui.Die("Missing command: " + cmd)
		}
	}
	if _, err := os.Stat(r.p.NodeToml); err != nil {
		return ui.Die("node.toml not found: " + r.p.NodeToml)
	}
	if _, err := os.Stat(r.p.EnvFile); err != nil {
		return ui.Die(".env not found: " + r.p.EnvFile)
	}
	pw := nodeconf.LoadKeystorePassword(r.p.EnvFile)
	if len(pw) == 0 {
		return ui.Die("KEYSTORE_PASSWORD not set in " + r.p.EnvFile)
	}
	defer ui.Zero(pw)
	r.tools = monad.Tools{Password: pw, EnvFile: r.p.EnvFile}

	if r.opt.Resume {
		res, err := r.st.LoadResume(r.p.BackupRoot, netinfo.ValidIPv4)
		switch {
		case errors.Is(err, state.ErrNoPreviousRun):
			r.c.Warn("No previous run found. Starting fresh.")
			r.opt.Resume = false
		case err != nil:
			return err
		default:
			r.c.OK(fmt.Sprintf("Resuming from step %d", res.LastStep+1))
			r.network, r.newSeq = res.Network, res.NewSeq
			r.secpPub, r.blsPub = res.SecpPub, res.BlsPub
			r.ip, r.selfAddress, r.selfSig = res.IP, res.SelfAddress, res.SelfSig
			r.selfSeq, r.selfAuthPort = res.SelfSeq, res.SelfAuthPort
			r.beneficiary, r.backupDir = res.Beneficiary, res.BackupDir
		}
	}

	// A resume can be hours old. While the cutover has not begun, the node's
	// health is worth re-reading rather than trusting the earlier result.
	if r.opt.Resume && r.st.Get("cutover_started") != "1" {
		if err := r.checkSync(); err != nil {
			return err
		}
	}

	if r.todo(1) {
		r.c.Phase(1, PhasesTotal, "PREFLIGHT")
		if err := r.checkSync(); err != nil {
			return err
		}
		if err := r.st.Set("last_step", "1"); err != nil {
			return err
		}
	}

	if r.todo(2) {
		r.c.Phase(2, PhasesTotal, "NETWORK & HOST")
		if err := r.detectNetwork(); err != nil {
			return err
		}
		if err := r.locationGuard(); err != nil {
			return err
		}
		if err := r.st.Set("network", r.network); err != nil {
			return err
		}
		if err := r.st.Set("last_step", "2"); err != nil {
			return err
		}
	}

	if r.todo(3) {
		r.c.Phase(3, PhasesTotal, "BACKUP CURRENT CONFIG")
		if err := r.backupConfig(); err != nil {
			return err
		}
		if err := r.st.Set("last_step", "3"); err != nil {
			return err
		}
	}

	if r.todo(4) {
		if err := r.importKeys(); err != nil {
			return err
		}
	}

	if r.todo(5) {
		if err := r.configure(); err != nil {
			return err
		}
	}

	if r.todo(6) {
		if err := r.signRecord(); err != nil {
			return err
		}
	}

	if r.todo(7) {
		if err := r.cutover(); err != nil {
			return err
		}
	}

	if r.todo(8) {
		if err := r.verify(); err != nil {
			return err
		}
	}

	return r.finish()
}

// todo reports whether phase n still has to run.
func (r *Run) todo(n int) bool {
	return !r.opt.Resume || !r.st.CompletedStep(n)
}

// ── phases 1–3 ───────────────────────────────────────────────────────

func (r *Run) checkSync() error {
	r.c.Step("NODE SYNC CHECK")
	if monad.Have("monad-status") {
		status, diff, _ := monad.Status()
		if status == "in-sync" {
			if diff == "" {
				diff = "0"
			}
			r.c.OK("Node: in-sync (block difference: " + diff + ")")
			return nil
		}
		if status == "" {
			status = "unknown"
		}
		return ui.Die("Node is " + status + ". Must be fully synced before promotion.")
	}
	r.c.Warn("monad-status not installed — cannot verify sync")
	if !r.c.ConfirmYN("continue without sync check?") {
		return ui.Die("Aborted.")
	}
	return nil
}

func (r *Run) detectNetwork() error {
	r.network = ""
	if data, err := os.ReadFile(r.p.NodeToml); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "network_name") {
				parts := strings.Split(line, `"`)
				if len(parts) >= 2 {
					r.network = parts[1]
				}
				break
			}
		}
	}
	if r.network != "mainnet" && r.network != "testnet" {
		ans, err := r.c.AskRaw("Network could not be detected. Enter (mainnet/testnet): ")
		if err != nil {
			return err
		}
		r.network = ans
		if r.network != "mainnet" && r.network != "testnet" {
			return ui.Die("Invalid network")
		}
	}
	r.c.OK("Network: " + r.network)
	return nil
}

// publicIP is the --public-ip override if given, otherwise the address
// detected over HTTPS, remembered for the rest of the run.
func (r *Run) publicIP() string {
	if r.opt.PublicIP != "" {
		return r.opt.PublicIP
	}
	if r.cachedIP == "" {
		r.cachedIP = netinfo.DetectPublicIPv4(r.p.IPURL)
	}
	return r.cachedIP
}

func (r *Run) locationGuard() error {
	host, _ := os.Hostname()
	r.c.Blank()
	r.c.Warn("This will " + ui.Bold + "promote this full node to validator" + ui.Reset + ".")
	r.c.Println("  Hostname:  " + ui.Bold + host + ui.Reset)
	if ip := r.publicIP(); ip != "" {
		r.c.Println("  Public IP: " + ui.Bold + ip + ui.Reset)
	}
	r.c.Blank()
	if !r.c.ConfirmYN("is this the correct target host?") {
		return ui.Die("Aborted.")
	}
	return nil
}

// backupConfig preserves this server's own identity (encrypted keystores and
// config) so it can be restored to a full node by hand. The keystore
// password is NOT copied: colocating it with the keys would defeat the
// encryption.
func (r *Run) backupConfig() error {
	r.backupDir = filepath.Join(r.p.BackupRoot, "failover-"+r.now().Format("20060102-150405"))
	if err := os.MkdirAll(r.backupDir, 0o700); err != nil {
		return ui.Die("Could not create " + r.backupDir)
	}
	_ = os.Chmod(r.backupDir, 0o700)
	for _, f := range []string{"node.toml", "id-secp", "id-bls"} {
		src := filepath.Join(r.p.ConfigDir, f)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := place.CopyPreserve(src, filepath.Join(r.backupDir, f)); err != nil {
			return ui.Die("Could not back up " + src + " to " + r.backupDir)
		}
	}
	if _, err := os.Stat(r.p.PubkeyList); err == nil {
		if err := place.CopyPreserve(r.p.PubkeyList, filepath.Join(r.backupDir, filepath.Base(r.p.PubkeyList))); err != nil {
			return ui.Die("Could not back up " + r.p.PubkeyList + " to " + r.backupDir)
		}
	}
	r.c.OK("Config backed up to " + r.backupDir)
	return r.st.Set("backup_dir", r.backupDir)
}

// ── phase 4: validator keys into staging ─────────────────────────────

func (r *Run) importKeys() error {
	for _, f := range []string{r.d.SecpNew, r.d.BlsNew, r.d.TomlNew} {
		_ = os.Remove(f)
	}
	r.c.Phase(4, PhasesTotal, "VALIDATOR KEY IMPORT")
	r.c.Println("  Keys are imported to staging files (id-secp.new / id-bls.new).")
	r.c.Println("  Live keys remain untouched until cutover.")
	r.c.Blank()

	src := r.opt.KeySourceDir
	if src == "" {
		r.c.Println("    1) Key backup files (secp-backup / bls-backup) — " + ui.Green + "recommended" + ui.Reset)
		r.c.Println("       Works even when the old server is unreachable.")
		r.c.Println("    2) Paste IKM hex values manually (hidden input)")
		r.c.Blank()
		choice, err := r.c.Ask("select (1/2)")
		if err != nil {
			return err
		}
		switch choice {
		case "1":
			dir, err := r.c.Ask("backup directory [" + r.p.BackupRoot + "]")
			if err != nil {
				return err
			}
			if dir == "" {
				dir = r.p.BackupRoot
			}
			src = dir
		case "2":
			src = "-"
		default:
			return ui.Die("Invalid selection")
		}
	}

	var secpIKM, blsIKM []byte
	defer func() { ui.Zero(secpIKM); ui.Zero(blsIKM) }()
	if src != "-" {
		r.c.Step("READ KEY BACKUP FILES")
		secpFile := filepath.Join(src, "secp-backup")
		blsFile := filepath.Join(src, "bls-backup")
		for _, f := range []string{secpFile, blsFile} {
			if fi, err := os.Stat(f); err != nil || !fi.Mode().IsRegular() {
				return ui.Die("Not found: " + f)
			}
		}
		var ok bool
		raw := nodeconf.ExtractIKMFromBackup(secpFile)
		secpIKM, ok = nodeconf.ValidateIKM(raw)
		ui.Zero(raw)
		if !ok {
			return ui.Die("Could not extract a valid SECP IKM from " + secpFile)
		}
		raw = nodeconf.ExtractIKMFromBackup(blsFile)
		blsIKM, ok = nodeconf.ValidateIKM(raw)
		ui.Zero(raw)
		if !ok {
			return ui.Die("Could not extract a valid BLS IKM from " + blsFile)
		}
		r.c.OK("IKM secrets extracted from backup files")
	} else {
		r.c.Println("  Paste the validator IKM hex values. Input is hidden.")
		r.c.Blank()
		raw, err := r.c.AskHidden("SECP IKM_HEX")
		if err != nil {
			return err
		}
		v, ok := nodeconf.ValidateIKM(raw)
		ui.Zero(raw)
		if !ok {
			return ui.Die("SECP IKM must be 64 hex characters")
		}
		secpIKM = v
		raw, err = r.c.AskHidden("BLS  IKM_HEX")
		if err != nil {
			return err
		}
		v, ok = nodeconf.ValidateIKM(raw)
		ui.Zero(raw)
		if !ok {
			return ui.Die("BLS IKM must be 64 hex characters")
		}
		blsIKM = v
	}

	r.c.Step("Importing SECP key (staging)")
	if err := r.tools.ImportKey(secpIKM, r.d.SecpNew); err != nil {
		return err
	}
	r.c.OK("SECP key imported to id-secp.new")
	r.c.Step("Importing BLS key (staging)")
	if err := r.tools.ImportKey(blsIKM, r.d.BlsNew); err != nil {
		return err
	}
	r.c.OK("BLS key imported to id-bls.new")
	ui.Zero(secpIKM)
	ui.Zero(blsIKM)

	r.secpPub = r.tools.RecoverPubkey(r.d.SecpNew, "secp")
	r.blsPub = r.tools.RecoverPubkey(r.d.BlsNew, "bls")
	if r.secpPub == "" {
		return ui.Die("Could not recover SECP public key")
	}
	if r.blsPub == "" {
		return ui.Die("Could not recover BLS public key")
	}

	r.c.Blank()
	r.c.Println("  SECP: " + ui.Bold + r.secpPub + ui.Reset)
	r.c.Println("  BLS:  " + ui.Bold + r.blsPub + ui.Reset)
	r.c.Blank()
	if !r.c.ConfirmYN("do these match your validator keys?") {
		return ui.Die("Key mismatch — aborting.")
	}
	r.c.OK("Keys verified")

	// Bind the confirmed keys to their bytes now. Cutover re-checks these and
	// refuses anything that changed after this confirmation.
	for _, kv := range [][2]string{
		{"secp_pub", r.secpPub},
		{"bls_pub", r.blsPub},
		{"staged_secp_sha", place.FileSHA(r.d.SecpNew)},
		{"staged_bls_sha", place.FileSHA(r.d.BlsNew)},
		{"last_step", "4"},
	} {
		if err := r.st.Set(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// ── phase 5: beneficiary, node name, sequence, flags (staging copy) ──

func (r *Run) configure() error {
	r.c.Phase(5, PhasesTotal, "CONFIGURE VALIDATOR")
	r.c.Println("  All changes go to a staging copy (node.toml.new).")
	r.c.Println("  The live config is untouched until cutover.")

	// Never touch the live node.toml before cutover. An abort at the STOPPED
	// gate must leave a fully unmodified full node behind.
	if err := place.CopyPreserve(r.p.NodeToml, r.d.TomlNew); err != nil {
		return ui.Die("Could not copy " + r.p.NodeToml + " to staging")
	}

	r.c.Blank()
	cur := nodeconf.TomlGet(r.d.TomlNew, "beneficiary")
	r.c.Println(ui.Bold + "BENEFICIARY" + ui.Reset)
	r.c.Println("Enter the beneficiary address from the old validator's node.toml.")
	r.c.Println("Leave blank to keep the address already in this node's config:")
	shown := cur
	if shown == "" {
		shown = "(none set)"
	}
	r.c.Println("    " + ui.Bold + shown + ui.Reset)
	ben, err := r.c.Ask("beneficiary")
	if err != nil {
		return err
	}
	if ben != "" {
		if !nodeconf.BeneficiaryRe.MatchString(ben) {
			return ui.Die("beneficiary must be a 0x-prefixed 40-hex-character address")
		}
		if err := nodeconf.SetTomlValue(r.d.TomlNew, "beneficiary", `"`+ben+`"`, ""); err != nil {
			return err
		}
		r.c.OK("Beneficiary: " + ben)
		if nodeconf.ZeroAddressRe.MatchString(ben) {
			r.c.Warn("You entered the ZERO address. This validator will have no beneficiary set.")
		}
		r.beneficiary = ben
	} else {
		// Blank means "keep what is there", so show exactly what that is and
		// get a yes for it. Rewards go to this address.
		if cur == "" {
			return ui.Die("No beneficiary given and none set in the config.",
				"Re-run and enter the validator's beneficiary address.")
		}
		if nodeconf.ZeroAddressRe.MatchString(cur) {
			r.c.Warn("The address already in the config is the ZERO address.")
			r.c.Println("  Keeping it means this validator has no beneficiary set.")
		}
		r.c.Println("  Keeping: " + ui.Bold + cur + ui.Reset)
		if !r.c.ConfirmYN("keep this beneficiary?") {
			return ui.Die("Aborted — re-run and enter the beneficiary address you want.")
		}
		r.beneficiary = cur
		r.c.OK("Beneficiary kept: " + cur)
	}

	r.c.Blank()
	r.c.Println(ui.Bold + "NODE NAME" + ui.Reset)
	r.c.Println("Per the migration docs, this node should take over the old validator's")
	r.c.Println("node_name during migration. Leave empty to keep the current name.")
	name, err := r.c.Ask("node_name")
	if err != nil {
		return err
	}
	if name != "" {
		if !nodeconf.NodeNameRe.MatchString(name) {
			return ui.Die("node_name may contain only letters, digits, dot, dash, underscore (max 64)")
		}
		if err := nodeconf.SetTomlValue(r.d.TomlNew, "node_name", `"`+name+`"`, ""); err != nil {
			return err
		}
		r.c.OK("node_name: " + name)
	} else {
		r.c.OK("node_name unchanged")
	}

	r.c.Blank()
	r.c.Println(ui.Bold + "SEQ NUM" + ui.Reset)
	r.c.Println("The name record's sequence number must be higher than any value this")
	r.c.Println("validator identity has used before. Gaps are harmless.")
	r.c.Blank()

	suggested := ""
	r.c.Step("FOUNDATION SNAPSHOT")
	body, fetchErr := foundation.Fetch(r.p.FoundationBase, r.network)
	res, note := foundation.Lookup(body, fetchErr, r.network, r.secpPub, r.blsPub, r.p.FoundationMaxAge, r.now())
	if note == "" {
		r.foundSeq, r.foundKnown = res.Seq, true
		suggested = strconv.FormatUint(res.Seq+1, 10)
		r.c.OK(fmt.Sprintf("Last published sequence for this key: %s%d%s (%s snapshot, %dh old)",
			ui.Bold, res.Seq, ui.Reset, r.network, res.AgeHours))
		r.c.Println("  Suggested for this migration: " + ui.Bold + suggested + ui.Reset)
		r.c.Println("  Press Enter to use it, or type a higher number if you know of a later one.")
	} else {
		r.c.Warn("Could not read a sequence from the Foundation snapshot (" + note + ").")
		r.c.Println("  Enter the value yourself: one higher than the last this identity used.")
		r.c.Println("  Check your records or the old validator's node.toml.")
	}

	label := "new seq_num"
	if suggested != "" {
		label += " [" + suggested + "]"
	}
	seq, err := r.c.Ask(label)
	if err != nil {
		return err
	}
	if seq == "" && suggested != "" {
		seq = suggested
	}
	if !seqInputRe.MatchString(seq) {
		return ui.Die("Must be a positive number")
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	if err != nil || n > foundation.SeqSaneMax {
		return ui.Die("Sequence number is unreasonably large")
	}
	// The snapshot value is a floor, never a ceiling: a stale snapshot can
	// only be behind the network, so anything at or below it would be rejected.
	if r.foundKnown && n <= r.foundSeq {
		return ui.Die(fmt.Sprintf("seq_num %s is not higher than the %d already published for this key.", seq, r.foundSeq),
			"Peers would reject the record. Use "+suggested+" or higher.")
	}
	r.newSeq = seq
	r.c.OK("seq_num for this migration: " + seq)

	for _, e := range []struct{ k, v, sec string }{
		{"enable_publisher", "true", "fullnode_raptorcast"},
		{"enable_client", "true", "fullnode_raptorcast"},
		{"expand_to_group", "true", "statesync"},
	} {
		if err := nodeconf.SetTomlValue(r.d.TomlNew, e.k, e.v, e.sec); err != nil {
			return err
		}
	}
	r.verifyConfigFlags(r.d.TomlNew)

	for _, kv := range [][2]string{
		{"beneficiary", r.beneficiary},
		{"new_seq", r.newSeq},
		{"last_step", "5"},
	} {
		if err := r.st.Set(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

func (r *Run) verifyConfigFlags(file string) {
	r.c.Step("VERIFY CONFIG FLAGS")
	if missing := nodeconf.MissingConfigFlags(file); len(missing) > 0 {
		r.c.Warn("Flags not set: " + strings.Join(missing, " "))
		r.c.Println("  The official migration docs require these to be true.")
		return
	}
	r.c.OK("enable_publisher, enable_client, expand_to_group all set")
}

// ── phase 6: sign the name record, patch the staged config ───────────

func (r *Run) signRecord() error {
	r.c.Phase(6, PhasesTotal, "SIGN NAME RECORD")
	if fi, err := os.Stat(r.d.TomlNew); err != nil || !fi.Mode().IsRegular() {
		return ui.Die("Staging config (node.toml.new) is missing.",
			"Start a fresh run so the configure step re-creates it.")
	}
	r.ip = r.publicIP()
	if r.ip == "" {
		return ui.Die("Could not detect a valid public IPv4 address.",
			"Retry with: "+r.opt.Argv0+" --resume --public-ip <this-server-public-IPv4>")
	}
	r.c.OK("Public IP: " + r.ip)

	if err := nodeconf.SanitizePlaceholders(r.d.TomlNew); err != nil {
		return err
	}
	r.c.OK("Placeholders sanitized")

	r.c.Step("SIGN NAME RECORD (seq " + r.newSeq + ")")
	out, err := r.tools.SignNameRecord(r.ip, r.newSeq, r.d.SecpNew)
	if err != nil {
		return err
	}
	signed, warning, err := monad.ParseSignerOutput(out, r.newSeq)
	if err != nil {
		return err
	}
	if warning != "" {
		r.c.Warn(warning)
		r.c.Println("  Safe to continue: the signature matches the emitted value.")
	}
	r.c.OK("Name record signed (seq " + signed.Seq + ")")
	r.selfAddress, r.selfAuthPort, r.selfSeq, r.selfSig = signed.Address, signed.AuthPort, signed.Seq, signed.Sig

	r.c.Step("PATCH node.toml")
	for _, e := range []struct{ k, v string }{
		{"self_address", `"` + signed.Address + `"`},
		{"self_auth_port", signed.AuthPort},
		{"self_record_seq_num", signed.Seq},
		{"self_name_record_sig", `"` + signed.Sig + `"`},
	} {
		if err := nodeconf.SetTomlValue(r.d.TomlNew, e.k, e.v, "peer_discovery"); err != nil {
			return err
		}
	}
	r.fixOwnership()
	r.c.OK("node.toml patched and verified")

	for _, kv := range [][2]string{
		{"ip", r.ip},
		{"self_address", r.selfAddress},
		{"self_sig", r.selfSig},
		{"self_seq", r.selfSeq},
		{"self_auth_port", r.selfAuthPort},
		// node.toml.new is final once the record is signed and patched in.
		{"staged_toml_sha", place.FileSHA(r.d.TomlNew)},
		{"last_step", "6"},
	} {
		if err := r.st.Set(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// fixOwnership hands the config directory and .env to the monad account and
// keeps the live keys private. Failures are ignored, as before: in the test
// sandbox there is no monad user and no permission to chown.
func (r *Run) fixOwnership() {
	uid, gid, err := monad.MonadIDs()
	if err == nil {
		_ = filepath.WalkDir(r.p.ConfigDir, func(p string, _ os.DirEntry, err error) error {
			if err == nil {
				_ = os.Lchown(p, uid, gid)
			}
			return nil
		})
		_ = os.Chown(r.p.EnvFile, uid, gid)
	}
	_ = os.Chmod(r.p.SecpKey, 0o600)
	_ = os.Chmod(r.p.BlsKey, 0o600)
}
