# Monad-failover

Single script to promote a synced Monad full node to validator.

Follows the official [Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.

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
12. Shows validator event monitoring commands and Hoodscan link

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

- Synced Monad Full node
- `monad-keystore`, `monad-sign-name-record` on PATH
- `aria2c` installed (for snapshot download)
- `/home/monad/.env` with `KEYSTORE_PASSWORD` set
- `monad-status` recommended (not required)

## Safety

- Refuses to run if the node is not in-sync
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
