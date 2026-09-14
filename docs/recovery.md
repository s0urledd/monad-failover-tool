# Recovering an interrupted migration

Start with `monad-failover --resume`. It verifies the saved files, finishes an
interrupted swap and retries service checks and backup export. Do not manually
start services while the tool reports mismatched keys or config.

## Common cases

- IP detection failed: use `monad-failover --resume --public-ip <target-IPv4>`.
- Services failed: inspect the named unit with `journalctl -xeu <unit>`, fix
  the failure and follow the printed start/resume commands. A unit you masked
  before the run is not automatically unmasked. Consensus and execution are
  required; an intentionally masked RPC unit can stay masked.
- Sync is pending: let the node catch up, then run `--resume` to recheck.
- Backup export failed: preserve the validator backup files you supplied and
  the timestamped `.bak` copies. Fix the reported cause, then run `--resume`.

Logs are in `/opt/monad/failover-logs/`. The current run's state is
`/var/lib/monad-failover/state`. Read it as text, never source it as shell code.

## Restoring this target to its previous full-node identity

This is a manual alternative when resume cannot finish. It replaces the target's
validator identity with the full-node identity saved before cutover. It does
**not** return the validator role to the old server, undo a published sequence,
or guarantee the restored node immediately syncs.

Use the specific `failover-<timestamp>` directory printed by the interrupted
run, not the top-level directory containing `secp-backup` and `bls-backup`.
The expected files are `node.toml`, `id-secp` and `id-bls`. You also need the
target's original `KEYSTORE_PASSWORD` in `/home/monad/.env`; the tool does not
back up that password.

Run these steps as root on the target using a Bash shell. Stop if any command
fails. These are the default installation paths; review any custom paths first.

1. Make sure no failover process is running. Hold its lock in this shell for
   the entire recovery, and record the pre-existing masks before changing them:

   ```bash
   exec 9>/var/lib/monad-failover/.lock
   flock -n 9
   grep -E '^(backup_dir|premasked_units|mask_observed)=' /var/lib/monad-failover/state
   ```

   If state is absent or untrusted, use the run log and your own service records.
   Do not guess which masks belonged to the operator.

2. Set `backup` to the exact directory for this run and check that all three
   files are present, regular files and not symlinks. Confirm that these are
   the original full-node backups before replacing anything:

   ```bash
   backup=/opt/monad/backup/failover-YYYYMMDD-HHMMSS
   config=/home/monad/monad-bft/config
   for file in node.toml id-secp id-bls; do
     test -f "$backup/$file" && test ! -L "$backup/$file" || break
     stat -c '%U %a %n' "$backup/$file"
   done
   ```

   All three must appear. Review `node.toml` locally, including the full-node
   name and settings. Do not paste key contents into support messages.

3. Mask and stop all three services. Confirm they are masked and none is active:

   ```bash
   systemctl mask monad-bft monad-execution monad-rpc
   systemctl stop monad-bft monad-execution monad-rpc
   systemctl is-enabled monad-bft monad-execution monad-rpc
   systemctl is-active monad-bft monad-execution monad-rpc
   ```

   An inactive result has a nonzero exit code. Do not copy files if any unit is
   still active or masking failed. Keep them masked across any interruption.

4. Restore the complete set, then compare every file before starting services:

   ```bash
   install -o monad -g monad -m 600 "$backup/id-secp" "$config/id-secp" &&
   install -o monad -g monad -m 600 "$backup/id-bls" "$config/id-bls" &&
   install -o monad -g monad -m 600 "$backup/node.toml" "$config/node.toml" &&
   cmp "$backup/id-secp" "$config/id-secp" &&
   cmp "$backup/id-bls" "$config/id-bls" &&
   cmp "$backup/node.toml" "$config/node.toml"
   ```

   If `pubkey-secp-bls` was backed up, restore that public-key listing to
   `/home/monad/pubkey-secp-bls` as well. If any copy or comparison fails, leave
   services masked and repeat the complete restore after fixing the cause.

5. Only after the complete set is verified, archive the old state outside the
   active state path. Do not resume the abandoned validator migration:

   ```bash
   archive=$(mktemp -d /var/lib/monad-failover/restored-XXXXXXXX)
   if test -f /var/lib/monad-failover/state; then
     mv /var/lib/monad-failover/state "$archive/state"
   fi
   if test -d /var/lib/monad-failover/staging; then
     mv /var/lib/monad-failover/staging "$archive/staging"
   fi
   ```

6. Unmask and start only the units that were not masked before migration.
   For example, if RPC was already masked but the other two were not:

   ```bash
   systemctl unmask monad-bft monad-execution &&
   systemctl start monad-bft monad-execution
   systemctl status monad-bft monad-execution
   monad-status
   ```

   If none was pre-masked, include `monad-rpc` in these commands. Preserve
   operator masks. Check logs and full-node sync, then release the lock with
   `exec 9>&-`. Retain the archived state and backup until recovery is verified.

To return the validator role to another server, follow a new migration with a
higher sequence. Stop the current validator before confirming that cutover. See
[Monad's restoration procedure](https://docs.monad.xyz/node-ops/node-recovery/node-migration#restoring-the-original-validator).

This manual restore guide has been reviewed against the backup/state layout;
it is not part of the recorded VM reboot tests.
