# monad-failover

Single script to promote a synced Monad full node to validator.

Follows the official [Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.
Turns a 15-step manual process into one guided command.

## What it does

1. Checks node is synced (`monad-status`)
2. Warns if RPC 8080 is publicly exposed
3. Backs up current config to `/opt/monad/backup/`
4. Downloads validator `node.toml` template
5. Asks for beneficiary address, `seq_num`, SECP IKM, BLS IKM (manual input)
6. Imports keys, verifies public keys
7. Signs name record with new seq_num
8. Patches `node.toml` (self_address, self_record_seq_num, self_name_record_sig)
9. Stops services, runs hard reset (snapshot restore)
10. Downloads forkpoint + validators.toml
11. Starts services

## About seq_num

The script asks you for the **last `self_record_seq_num`** from the failed validator's `node.toml`.
It then uses that value + 1 for the new name record.

Example: if the failed validator had `self_record_seq_num = 3`, enter `3`. The script will use `4`.

This value is tied to the name record signature. Getting it wrong will prevent the node from joining consensus.

## About keys

You enter SECP and BLS `IKM_HEX` values manually. Input is hidden.

After import, the script recovers and displays the public keys so you can verify they match your validator.

Keys are never stored in the script or written to disk outside the keystore.

## Requirements

- Synced full node ([installation guide](https://docs.monad.xyz/node-ops/full-node-installation))
- Monad >= 0.12.x
- Linux kernel >= 6.8.0.60 (v6.8.0.56-59 has a known Monad hang bug)
- `monad-keystore`, `monad-sign-name-record` on PATH
- `aria2c` installed (for snapshot download)
- `/home/monad/.env` with `KEYSTORE_PASSWORD` set
- `monad-status` recommended (not required)

## Install

```
cd /home/monad
curl -sSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/main/monad-failover.sh
chmod +x monad-failover.sh
```

## Usage

```
./monad-failover.sh
```

The script detects mainnet/testnet from `network_name` in `node.toml` and uses the correct snapshot and config URLs.

## Example output

```
╔══════════════════════════════════════════════╗
║        MONAD VALIDATOR FAILOVER TOOL         ║
╚══════════════════════════════════════════════╝

▶ NODE SYNC CHECK
✔ Node status: in-sync (block difference: 0)

▶ RPC SECURITY CHECK
✔ RPC not publicly exposed

✔ Network: mainnet

This will promote this full node to validator.
Make sure the original validator services are STOPPED.

Continue? (y/N): y

▶ BACKUP CURRENT CONFIG
✔ Config backed up to /opt/monad/backup/failover-20260305-141200

▶ DOWNLOAD VALIDATOR node.toml
✔ Validator node.toml downloaded

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
BENEFICIARY
Enter the beneficiary address from the failed validator's node.toml
beneficiary: 0x1234...abcd
✔ Beneficiary set: 0x1234...abcd

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
SEQ NUM
Enter last seq_num from failed validator's node.toml (example: 0)
seq_num: 0
✔ New seq_num: 1

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
KEY IMPORT
Paste IKM hex values. Input is hidden.

SECP IKM_HEX:
BLS  IKM_HEX:

▶ Importing SECP key
✔ SECP key imported

▶ Importing BLS key
✔ BLS key imported

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
VERIFY KEYS
[14:12:05] INFO  SECP Pubkey: 03bbf692002bda53050f22289d4da8fe0bec8b81...
[14:12:05] INFO  BLS  Pubkey: 985d3f7052ac5ad586592ba1a240b0260b5351a9...

Do these match your validator keys? (y/N): y
✔ Keys verified

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
NODE ADDRESS
Detected IP: 65.109.145.172
Use this IP? (Y/n): y
✔ Using: 65.109.145.172

▶ SIGN NAME RECORD
✔ Name record signed

▶ PATCH node.toml
✔ node.toml updated

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
SUMMARY
  Network:     mainnet
  IP:          65.109.145.172:8000
  seq_num:     1
  Beneficiary: 0x1234...abcd
  SECP key:    03bbf692002bda53...
  BLS  key:    985d3f7052ac5ad5...

Proceed with hard reset and start? (y/N): y

▶ STOP SERVICES
✔ Services stopped

▶ HARD RESET
Resetting workspace...
Restoring snapshot (mainnet)...
Downloading forkpoint...
Downloading validators.toml...
✔ Hard reset complete

▶ START SERVICES
✔ Services started

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
✔ VALIDATOR PROMOTION COMPLETE
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Check status:
  systemctl status monad-bft monad-execution monad-rpc --no-pager -l
  journalctl -fu monad-bft
  monad-status
```

## Safety

- Refuses to run if the node is not in-sync
- Refuses to run on known-buggy kernel versions (6.8.0.56-59)
- Warns if RPC 8080 is exposed to the internet
- Backs up existing config (including pubkey-secp-bls) before any changes
- Keys are entered manually and never logged
- Confirms at every critical step before proceeding

## After promotion

If you have downstream full nodes connected to this validator, they must update the
validator's name record in their `node.toml` to maintain connectivity.

## Reference

- [Full Node Installation](https://docs.monad.xyz/node-ops/full-node-installation)
- [Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration)
- [Hard Reset](https://docs.monad.xyz/node-ops/node-recovery/hard-reset)

## License

MIT
