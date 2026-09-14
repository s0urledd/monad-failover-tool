package promote

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/s0urledd/monad-failover-tool/internal/monad"
	"github.com/s0urledd/monad-failover-tool/internal/place"
	"github.com/s0urledd/monad-failover-tool/internal/rpcports"
	"github.com/s0urledd/monad-failover-tool/internal/systemd"
	"github.com/s0urledd/monad-failover-tool/internal/ui"
	"github.com/s0urledd/monad-failover-tool/internal/uptime"
)

func (r *Run) restoreFrom() string {
	if r.backupDir != "" {
		return r.backupDir
	}
	return r.p.BackupRoot
}

func truncate24(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s + "..."
}

// ── phase 7: confirm the old validator is stopped, then swap ─────────

func (r *Run) cutover() error {
	r.c.Phase(7, PhasesTotal, "CUTOVER")
	if r.st.Get("swap_done") != "1" {
		host, _ := os.Hostname()
		seq := r.selfSeq
		if seq == "" {
			seq = r.newSeq
		}
		ben := r.beneficiary
		if ben == "" {
			ben = "not set"
		}
		r.c.Blank()
		r.c.Println("┌─ PROMOTION SUMMARY ────────────────────────────────────────")
		r.c.Printf("│  %-12s %s\n", "hostname", host)
		r.c.Printf("│  %-12s %s\n", "network", r.network)
		r.c.Printf("│  %-12s %s\n", "address", r.selfAddress)
		r.c.Printf("│  %-12s %s\n", "seq_num", seq)
		r.c.Printf("│  %-12s %s\n", "beneficiary", ben)
		r.c.Printf("│  %-12s %s\n", "secp", truncate24(r.secpPub))
		r.c.Printf("│  %-12s %s\n", "bls", truncate24(r.blsPub))
		r.c.Println("└────────────────────────────────────────────────────────────")
		r.c.Blank()

		r.c.Warn("Stop the old validator before confirming cutover.")
		r.c.Println("      " + ui.Bold + "systemctl stop monad-bft monad-execution monad-rpc" + ui.Reset)
		r.c.Blank()
		ans, err := r.c.Ask("type STOPPED to confirm")
		if err != nil {
			return err
		}
		if ans != "STOPPED" {
			return ui.Die("Not confirmed — aborting before cutover.")
		}
		r.c.OK("Old validator confirmed stopped or offline")

		r.c.Blank()
		r.c.Println("  Preparation is complete. Confirm when you are ready to begin the switch.")
		r.c.Println("  The tool will verify the prepared files, replace the identity and start services.")
		r.c.Blank()
		if !r.c.ConfirmYN("proceed with cutover?") {
			return ui.Die("Aborted.")
		}

		// Every file must still match the checksum recorded when it was
		// prepared and confirmed, or already be in place from an earlier
		// attempt, before any service is touched.
		if err := place.VerifyStagedOrPlaced(r.d.SecpNew, r.p.SecpKey, r.st.Get("staged_secp_sha"), "the SECP key"); err != nil {
			return err
		}
		if err := place.VerifyStagedOrPlaced(r.d.BlsNew, r.p.BlsKey, r.st.Get("staged_bls_sha"), "the BLS key"); err != nil {
			return err
		}
		if err := place.VerifyStagedOrPlaced(r.d.TomlNew, r.p.NodeToml, r.st.Get("staged_toml_sha"), "node.toml"); err != nil {
			return err
		}

		// From here on the run mutates the live node: mark it, so a later run
		// without --resume refuses to start fresh over a half-swapped identity.
		if err := r.st.Set("cutover_started", "1"); err != nil {
			return err
		}
		if err := r.maskServices(); err != nil {
			return err
		}
		if err := r.stopServices(); err != nil {
			return err
		}

		resume := r.opt.Argv0 + " --resume"
		if err := r.placeOne(r.d.SecpNew, r.p.SecpKey, "staged_secp_sha", "SECP key"); err != nil {
			if errors.Is(err, place.ErrNotPlaced) {
				return ui.Die("Could not place the SECP key. Services are stopped; nothing has changed.",
					"Fix the cause, then continue with: "+resume)
			}
			return err
		}
		if err := r.placeOne(r.d.BlsNew, r.p.BlsKey, "staged_bls_sha", "BLS key"); err != nil {
			if errors.Is(err, place.ErrNotPlaced) {
				return ui.Die("CRITICAL: the SECP key was placed but the BLS key was not.",
					"This node now has a mismatched identity. Do NOT start the services.",
					"Fix the cause, then continue with: "+resume,
					"(it finishes placing the remaining files). To roll back instead,",
					"restore this node's previous identity from: "+r.restoreFrom())
			}
			return err
		}
		if err := r.placeOne(r.d.TomlNew, r.p.NodeToml, "staged_toml_sha", "node.toml"); err != nil {
			if errors.Is(err, place.ErrNotPlaced) {
				return ui.Die("CRITICAL: the keys were placed but node.toml was not.",
					"Do NOT start the services with this key/config mismatch.",
					"Fix the cause, then continue with: "+resume,
					"(it finishes placing node.toml). To roll back instead, restore",
					"this node's previous identity from: "+r.restoreFrom())
			}
			return err
		}
		r.fixOwnership()

		// Stage boundary: the files are in place. Bringing the services back
		// up is a separately resumable step, so an interruption here cannot
		// leave the units masked with nothing left to unmask them.
		if err := r.st.Set("swap_done", "1"); err != nil {
			return err
		}
	} else {
		r.c.OK("Files were already swapped by an earlier attempt; bringing services up")
	}

	// ── 7b: unmask and start (resumable on its own) ──
	if err := r.verifyLiveIdentity(); err != nil {
		return err
	}
	if err := r.unmaskServices(); err != nil {
		return err
	}
	svcs := r.startableServices()
	if len(svcs) == 0 {
		r.c.Warn("Every monad unit was already masked before this run; not starting any.")
	} else {
		systemd.Enable(svcs...)
		if err := systemd.Start(r.c.Out, r.c.Err, svcs...); err != nil {
			return ui.Die("The validator keys are in place, but the services failed to start.",
				"The swap is done — do NOT re-run the cutover.",
				"Diagnose: journalctl -xeu monad-bft",
				"Start when fixed: systemctl start "+strings.Join(svcs, " "),
				"Then finish up:  "+r.opt.Argv0+" --resume")
		}
		r.c.OK("Services started")
	}

	// Only now is step 7 complete: files placed AND services up.
	return r.st.Set("last_step", "7")
}

func (r *Run) placeOne(staged, live, key, label string) error {
	already, err := place.Verified(staged, live, r.st.Get(key), label, r.restoreFrom())
	if err != nil {
		return err
	}
	if already {
		r.c.OK(label + " already in place from a previous cutover attempt")
	} else {
		r.c.OK(label + " placed")
	}
	return nil
}

func premasked(pre string) map[string]bool {
	m := map[string]bool{}
	for _, u := range strings.Fields(pre) {
		m[u] = true
	}
	return m
}

// maskServices masks the units before the swap. A reboot between the three
// file swaps would otherwise let systemd start the units with a half-swapped
// identity: stopping a unit does not stop it coming back on boot. Masking
// does, and it survives a reboot (a --runtime mask does not). Units already
// masked before this run are recorded so they are not unmasked afterwards.
func (r *Run) maskServices() error {
	r.c.Step("MASK MONAD SERVICES")
	// Record what the operator had already masked BEFORE masking anything.
	// If the observation were written after the first mask, an interruption
	// in between would leave a resume reading this run's own masks as the
	// operator's, and it would then refuse to unmask them.
	if r.st.Get("mask_observed") != "1" {
		var pre []string
		for _, u := range systemd.Units {
			if systemd.IsMasked(u) {
				pre = append(pre, u)
			}
		}
		if err := r.st.Set("premasked_units", strings.Join(pre, " ")); err != nil {
			return err
		}
		if err := r.st.Set("mask_observed", "1"); err != nil {
			return err
		}
	}
	systemd.Mask(systemd.Units...)
	for _, u := range systemd.Units {
		if !systemd.IsMasked(u) {
			return ui.Die("Could not mask "+u+" — refusing to start the swap.",
				"Without a mask, a reboot mid-swap would start this node with a",
				"half-swapped identity. Nothing has been changed.",
				"Check: systemctl mask "+strings.Join(systemd.Units, " "))
		}
	}
	if err := r.st.Set("services_masked", "1"); err != nil {
		return err
	}
	r.c.OK("Services masked for the swap")
	return nil
}

func (r *Run) stopServices() error {
	r.c.Step("STOP MONAD SERVICES")
	systemd.Stop(r.c.Out, systemd.Units...)
	r.sleep(time.Second)
	still := false
	for _, u := range systemd.Units {
		if systemd.IsActive(u) {
			still = true
			r.c.Warn(u + " still running")
		}
	}
	if still {
		return ui.Die("Could not stop all services")
	}
	r.c.OK("Services stopped")
	return nil
}

// unmaskServices unmasks only what this run masked; a unit the operator had
// masked beforehand stays masked.
func (r *Run) unmaskServices() error {
	pre := premasked(r.st.Get("premasked_units"))
	var failed []string
	for _, u := range systemd.Units {
		if pre[u] {
			continue
		}
		systemd.Unmask(u)
		if systemd.IsMasked(u) {
			failed = append(failed, u)
		}
	}
	if len(failed) > 0 {
		return ui.Die("Could not unmask: "+strings.Join(failed, " "),
			"The keys and config are in place but these units cannot start while",
			"masked. Unmask them, then finish with: "+r.opt.Argv0+" --resume")
	}
	return r.st.Set("services_masked", "0")
}

// verifyLiveIdentity: the swap may have happened in an earlier run, possibly
// long ago. Nothing is started until all three live files still match what
// was placed.
func (r *Run) verifyLiveIdentity() error {
	r.c.Step("VERIFY PLACED IDENTITY")
	if err := place.CheckLive(r.p.SecpKey, r.st.Get("staged_secp_sha"), "the SECP key", r.restoreFrom()); err != nil {
		return err
	}
	if err := place.CheckLive(r.p.BlsKey, r.st.Get("staged_bls_sha"), "the BLS key", r.restoreFrom()); err != nil {
		return err
	}
	if err := place.CheckLive(r.p.NodeToml, r.st.Get("staged_toml_sha"), "node.toml", r.restoreFrom()); err != nil {
		return err
	}
	r.c.OK("Live identity matches what was placed")
	return nil
}

// startableServices leaves out units the operator had masked before this
// run, so they are not started either.
func (r *Run) startableServices() []string {
	pre := premasked(r.st.Get("premasked_units"))
	var out []string
	for _, u := range systemd.Units {
		if !pre[u] {
			out = append(out, u)
		}
	}
	return out
}

// ── phase 8: verify, uptime, fresh backups ───────────────────────────

func (r *Run) verify() error {
	r.c.Phase(8, PhasesTotal, "VERIFY")
	if err := r.postVerify(); err != nil {
		return err
	}
	r.checkUptimeAPI()
	if err := r.refreshKeyBackups(); err != nil {
		return ui.Die("Key backup export failed after an otherwise successful promotion.",
			"The validator itself is live — nothing else is wrong. Previous backup",
			"copies are preserved as *.bak in "+r.p.BackupRoot+".",
			"Retry just this export with: "+r.opt.Argv0+" --resume")
	}
	if !r.verifyPending {
		return r.st.Set("last_step", "8")
	}
	return nil
}

// postVerify is the hard health gate after cutover: `systemctl start`
// returning success does not mean the services survived their first
// seconds. Wait, then require consensus and execution to be active. RPC may
// be deliberately masked by the operator; that choice is preserved without
// treating a stopped validator as healthy.
func (r *Run) postVerify() error {
	r.c.Step("POST-CUTOVER VERIFICATION")
	r.sleep(r.p.HealthWait)
	pre := premasked(r.st.Get("premasked_units"))
	resume := r.opt.Argv0 + " --resume"
	for _, u := range systemd.Units {
		if u == "monad-rpc" && pre[u] {
			r.c.Println("  monad-rpc was masked before this run; left unchanged.")
			continue
		}
		if pre[u] && !systemd.IsActive(u) {
			return ui.Die(u+" was already masked, but is required for validation.",
				"It has not been unmasked automatically. After checking why it was masked:",
				"  systemctl unmask "+u+" && systemctl start "+u,
				"Then finish: "+resume)
		}
		if !systemd.IsActive(u) {
			return ui.Die(u+" is not active after cutover.",
				"Check:  journalctl -xeu "+u,
				"Start:  systemctl start "+u,
				"Finish: "+resume)
		}
	}
	r.c.OK("All required services active")

	// Active units and a synced, participating node are two different
	// results. Give sync a bounded window instead of judging it five seconds
	// in, and if it still has not caught up, say so rather than declaring
	// success.
	r.verifyPending = false
	if !monad.Have("monad-status") {
		r.verifyPending = true
		r.c.Warn("monad-status not installed — sync could not be confirmed.")
		return nil
	}
	limit := r.p.SyncWait
	var waited time.Duration
	var status string
	for {
		status, _, _ = monad.Status()
		if status == "in-sync" {
			r.c.OK("Node is in-sync")
			return nil
		}
		if waited >= limit {
			break
		}
		r.sleep(5 * time.Second)
		waited += 5 * time.Second
	}
	r.verifyPending = true
	if status == "" {
		status = "no status"
	}
	r.c.Warn(fmt.Sprintf("Node reports %s after %ds — not in-sync yet.", status, int(limit/time.Second)))
	return nil
}

// checkUptimeAPI asks the network how it sees this validator. It never
// blocks the flow.
func (r *Run) checkUptimeAPI() {
	r.c.Step("VALIDATOR UPTIME CHECK")
	base := r.p.UptimeTestnet
	if r.network == "mainnet" {
		base = r.p.UptimeMainnet
	}
	url := uptime.URL(base, r.secpPub)
	rep, ok := uptime.Fetch(url)
	if !ok {
		r.c.Warn("Validator not visible in the uptime API yet (this can take a few minutes).")
		r.c.Println("  Check later: " + url)
		return
	}
	name := rep.Name
	if name == "" {
		name = "validator"
	}
	if rep.Status == "active" {
		r.c.OK(name + " is " + ui.Bold + rep.Status + ui.Reset + " on " + r.network + " (uptime API)")
	} else {
		status := rep.Status
		if status == "" {
			status = "unknown"
		}
		r.c.Warn(name + " is " + status + " on " + r.network + " (uptime API).")
		r.c.Println("  Check later: " + url)
	}
	or := func(s string) string {
		if s == "" {
			return "?"
		}
		return s
	}
	r.c.Printf("  Uptime (24h): %s%% (%s finalized, %s timeout)\n", or(rep.UptimePercent), or(rep.Finalized), or(rep.Timeout))
	if rep.LastRound != "" {
		r.c.Println("  Last round:   " + rep.LastRound)
	}
}

// refreshKeyBackups re-exports official-format key backups from the live
// validator keys. It returns an error so the caller keeps the resume state;
// otherwise the printed --resume advice would find nothing to resume.
func (r *Run) refreshKeyBackups() error {
	r.c.Step("REFRESH KEY BACKUPS")
	if err := os.MkdirAll(r.p.BackupRoot, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(r.p.BackupRoot, 0o700)
	ts := r.now().Format("20060102150405")
	se := filepath.Join(r.p.BackupRoot, "secp-backup")
	bl := filepath.Join(r.p.BackupRoot, "bls-backup")
	for _, f := range []string{se, bl} {
		if _, err := os.Stat(f); err == nil {
			_ = os.Rename(f, f+"."+ts+".bak")
		}
	}
	err := r.tools.ExportBackup(r.p.SecpKey, "secp", se)
	if err == nil {
		err = r.tools.ExportBackup(r.p.BlsKey, "bls", bl)
	}
	if err != nil {
		r.c.Warn("Could not re-export key backups.")
		r.c.Println("  Previous copies are preserved as *." + ts + ".bak in " + r.p.BackupRoot + ".")
		return err
	}
	r.c.OK("Key backups exported: " + r.p.BackupRoot + "/{secp-backup,bls-backup}")
	r.c.Warn("Store copies of both files OUTSIDE this server (password manager / vault).")
	r.c.Println("  These files contain unencrypted secret keys. Anyone holding them can use this identity.")
	return nil
}

// ── done ─────────────────────────────────────────────────────────────

func (r *Run) finish() error {
	for _, f := range []string{r.d.SecpNew, r.d.BlsNew, r.d.TomlNew} {
		_ = os.Remove(f)
	}

	// The swap succeeded either way; only sync is unconfirmed. Keep the
	// resume state so a later run re-checks, and do not print an
	// unconditional success.
	if r.verifyPending {
		r.c.Blank()
		r.c.Warn(ui.Bold + "CUTOVER COMPLETE — VERIFICATION PENDING" + ui.Reset)
		r.c.Println("  The validator keys and config are in place and the services are")
		r.c.Println("  running. The node has not reported in-sync yet, which is normal for")
		r.c.Println("  a short while after a migration.")
		r.c.Blank()
		r.c.Println("  Nothing to do now. Re-check when you want:  " + ui.Bold + r.opt.Argv0 + " --resume" + ui.Reset)
		r.c.Blank()
		return nil
	}

	r.st.Clear()
	r.c.Blank()
	r.c.Println(ui.Green + "════════════════════════════════════════════════════════════" + ui.Reset)
	r.c.Println("   " + ui.Green + "✔" + ui.Reset + "  " + ui.Bold + "VALIDATOR PROMOTION COMPLETE" + ui.Reset)
	r.c.Println(ui.Green + "════════════════════════════════════════════════════════════" + ui.Reset)

	r.c.Blank()
	r.c.Println(ui.Bold + "NODE STATUS" + ui.Reset)
	r.c.Println("journalctl -fu monad-bft")

	r.c.Blank()
	r.c.Println(ui.Bold + "VALIDATOR EVENTS" + ui.Reset)
	r.c.Println(`journalctl -u monad-ledger-tail -o cat -f | grep -i "` + r.secpPub + `"`)

	r.c.Blank()
	r.c.Warn("If you have downstream full nodes, update this validator's")
	r.c.Println("  name record in their node.toml to maintain connectivity.")

	r.rpcClosingNote()

	r.c.Blank()
	r.c.Warn("VDP: validators are required to push metrics to Monad Foundation's")
	r.c.Println("  monitoring infrastructure. Make sure this server is pushing them:")
	r.c.Println("  https://docs.monad.xyz/node-ops/validator-delegation-program")
	r.c.Blank()
	return nil
}

// rpcClosingNote is raised after the promotion, alongside the other things
// the operator now has to go and do. By this point the machine is a
// validator, which is when an exposed RPC port actually matters.
func (r *Run) rpcClosingNote() {
	r.c.Blank()
	r.c.Warn("Block public access to RPC and metrics ports (8080, 8081, 9143, etc.).")
	r.c.Println("  Allow trusted sources only.")
}

// portsText renders the checked port list for messages.
func portsText() string {
	parts := make([]string, len(rpcports.Ports))
	for i, p := range rpcports.Ports {
		parts[i] = fmt.Sprint(p)
	}
	return strings.Join(parts, " ")
}
