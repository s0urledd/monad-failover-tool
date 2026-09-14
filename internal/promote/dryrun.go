package promote

import (
	"fmt"
	"os"
	"strings"

	"github.com/s0urledd/monad-failover-tool/internal/monad"
	"github.com/s0urledd/monad-failover-tool/internal/nodeconf"
	"github.com/s0urledd/monad-failover-tool/internal/paths"
	"github.com/s0urledd/monad-failover-tool/internal/rpcports"
	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// checkRPC reports RPC listeners on non-loopback addresses. The dry run
// asks this before anything has changed, while the box is still a full
// node. The live run does not: an exposed port blocks nothing, and a warning
// at step 1 asks an operator mid-migration to stop and think about
// firewalls. It is raised at the end instead. Returns the warning count.
func checkRPC(c *ui.Console) int {
	c.Step("RPC EXPOSURE CHECK")
	exposed, err := rpcports.Exposed()
	if err != nil {
		c.Warn("Could not read the kernel socket tables — cannot check RPC exposure.")
		c.Println("  Verify manually that none of these ports listen publicly: " + portsText())
		return 1
	}
	if len(exposed) > 0 {
		parts := make([]string, len(exposed))
		for i, p := range exposed {
			parts[i] = fmt.Sprint(p)
		}
		c.Warn("RPC ports listening on non-loopback interfaces: " + strings.Join(parts, " "))
		c.Println("  Validators should not expose RPC publicly. If a firewall (ufw etc.)")
		c.Println("  already blocks these ports from outside, you are fine as is.")
		c.Println("  Otherwise bind them to localhost or block them now.")
		return 1
	}
	c.OK("No non-loopback RPC listeners found (checked: " + portsText() + "; firewall not checked)")
	return 0
}

// DryRun is the read-only preflight. It changes nothing and returns the
// process exit code: 1 when a blocking issue was found.
func DryRun(c *ui.Console, p paths.Paths, keySourceDir, version string) int {
	c.Header(version)
	c.Println(ui.Bold + "DRY RUN" + ui.Reset + " — read-only preflight. No files, keys or services are touched.")
	fails, warns := 0, 0

	c.Step("REQUIRED COMMANDS")
	for _, cmd := range []string{"systemctl", "monad-keystore", "monad-sign-name-record"} {
		if monad.Have(cmd) {
			c.OK(cmd)
		} else {
			c.Cross("missing: " + cmd)
			fails++
		}
	}
	if monad.Have("monad-status") {
		c.OK("monad-status")
	} else {
		c.Warn("monad-status not installed — sync gate will need manual confirmation")
		warns++
	}

	c.Step("FILES & ENVIRONMENT")
	if _, err := os.Stat(p.NodeToml); err == nil {
		c.OK("node.toml")
	} else {
		c.Cross("missing: " + p.NodeToml)
		fails++
	}
	if _, err := os.Stat(p.EnvFile); err == nil {
		c.OK(".env")
		if nodeconf.LoadKeystorePassword(p.EnvFile) != "" {
			c.OK("KEYSTORE_PASSWORD set")
		} else {
			c.Cross("KEYSTORE_PASSWORD not set in " + p.EnvFile)
			fails++
		}
	} else {
		c.Cross("missing: " + p.EnvFile)
		fails++
	}

	c.Step("SYNC STATUS")
	if monad.Have("monad-status") {
		status, diff, _ := monad.Status()
		if status == "in-sync" {
			if diff == "" {
				diff = "0"
			}
			c.OK("in-sync (block difference: " + diff + ")")
		} else {
			if status == "" {
				status = "unknown"
			}
			c.Cross("node is " + status + " — must be in-sync before promotion")
			fails++
		}
	} else {
		c.Warn("cannot verify sync without monad-status")
		warns++
	}

	warns += checkRPC(c)

	c.Step("KEY BACKUP FILES")
	c.Println("  Format check only: validator identity is NOT verified.")
	c.Println("  These may be this full node's own backups. Select the validator's backups in the live run.")
	dir := keySourceDir
	if dir == "" || dir == "-" {
		dir = p.BackupRoot
	}
	for _, f := range []string{"secp-backup", "bls-backup"} {
		path := dir + "/" + f
		if _, err := os.Stat(path); err == nil {
			if _, ok := nodeconf.ValidateIKM(nodeconf.ExtractIKMFromBackup(path)); ok {
				c.OK(path + " (valid IKM format; validator identity NOT verified)")
			} else {
				c.Warn(path + " exists but contains no valid IKM")
				warns++
			}
		} else {
			c.Warn(path + " not found — manual IKM entry would be required")
			warns++
		}
	}

	if _, err := os.Stat(p.NodeToml); err == nil {
		c.Step("VERIFY CONFIG FLAGS")
		if missing := nodeconf.MissingConfigFlags(p.NodeToml); len(missing) > 0 {
			c.Warn("Flags not set: " + strings.Join(missing, " "))
			c.Println("  The official migration docs require these to be true.")
		} else {
			c.OK("enable_publisher, enable_client, expand_to_group all set")
		}
	}

	c.Step("PLANNED ACTIONS (live run would do)")
	c.Println("  1. Back up this server's keys and config to " + p.BackupRoot + "/failover-<timestamp>/")
	c.Println("  2. Import validator keys to staging files (id-secp.new / id-bls.new)")
	c.Println("  3. Set node_name, beneficiary and config flags on a staging copy (node.toml.new)")
	c.Println("  4. Sign the name record with the seq_num you enter (used verbatim)")
	c.Println("     and patch the staged node.toml.new")
	c.Println("  5. After confirming the old validator is stopped: stop services,")
	c.Println("     swap the staged keys and config into place (resumable if")
	c.Println("     interrupted mid-swap), restart as validator")
	c.Println("  6. Verify every service is active, then re-export fresh key backups")

	c.Blank()
	c.Bar()
	if fails > 0 {
		c.Println(ui.Red + "✗" + ui.Reset + " " + ui.Bold + "Preflight failed" + ui.Reset +
			fmt.Sprintf(" — %d blocking issue(s), %d warning(s).", fails, warns))
		return 1
	}
	c.OK(ui.Bold + "Preflight passed" + ui.Reset + fmt.Sprintf(" — %d warning(s). Review warnings before the live run.", warns))
	c.Println("  Signing and cutover are checked during the live run, not this dry run.")
	c.Blank()
	return 0
}
