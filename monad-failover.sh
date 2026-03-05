#!/usr/bin/env bash
#
# monad-failover — promote a synced full node to validator
#
# Usage:
#   chmod +x monad-failover.sh
#   ./monad-failover.sh
#
# Manual run only. No daemon, no auto-failover.
# Prompts for: seq_num, SECP IKM, BLS IKM.
# Automates: key import, name record, node.toml patch, hard reset, service restart.

set -euo pipefail

# ── paths ──────────────────────────────────────────────
MONAD_HOME="/home/monad"
CONFIG_DIR="$MONAD_HOME/monad-bft/config"
NODE_TOML="$CONFIG_DIR/node.toml"
ENV_FILE="$MONAD_HOME/.env"
SECP_KEY="$CONFIG_DIR/id-secp"
BLS_KEY="$CONFIG_DIR/id-bls"
VALIDATORS_FILE="$CONFIG_DIR/validators/validators.toml"
MF_BUCKET="https://bucket.monadinfra.com"

# ── ui helpers ─────────────────────────────────────────
BOLD=$'\033[1m'
DIM=$'\033[2m'
RED=$'\033[31m'
GREEN=$'\033[32m'
YELLOW=$'\033[33m'
CYAN=$'\033[36m'
RESET=$'\033[0m'

header() {
  echo
  echo "╔══════════════════════════════════════════════╗"
  echo "║        MONAD VALIDATOR FAILOVER TOOL         ║"
  echo "╚══════════════════════════════════════════════╝"
  echo
}

bar()  { echo "${DIM}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"; }
step() { echo; echo "${CYAN}▶${RESET} ${BOLD}$*${RESET}"; }
ok()   { echo "${GREEN}✔${RESET} $*"; }
warn() { echo "${YELLOW}⚠${RESET} $*"; }
die()  { echo "${RED}✗${RESET} $*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || die "Missing command: $1"; }

# ── preflight ──────────────────────────────────────────
need_cmd curl
need_cmd systemctl
need_cmd sed
need_cmd awk
need_cmd aria2c
need_cmd monad-keystore
need_cmd monad-sign-name-record

[ -f "$NODE_TOML" ] || die "node.toml not found: $NODE_TOML"
[ -f "$ENV_FILE" ]  || die ".env not found: $ENV_FILE"

# ── start ──────────────────────────────────────────────
clear 2>/dev/null || true
header

# shellcheck disable=SC1090
source "$ENV_FILE"
[ -n "${KEYSTORE_PASSWORD:-}" ] || die "KEYSTORE_PASSWORD not set in $ENV_FILE"

# ── node sync check ───────────────────────────────────
step "NODE SYNC CHECK"

if command -v monad-status >/dev/null 2>&1; then
  STATUS_OUT="$(monad-status 2>/dev/null || true)"
  SYNC_STATUS="$(echo "$STATUS_OUT" | grep -m1 'status:' | awk '{print $2}')"
  BLOCK_DIFF="$(echo "$STATUS_OUT" | grep -m1 'blockDifference:' | awk '{print $2}')"

  if [[ "$SYNC_STATUS" == "in-sync" ]]; then
    ok "Node status: in-sync (block difference: ${BLOCK_DIFF:-0})"
  else
    warn "Node status: ${SYNC_STATUS:-unknown}"
    echo
    echo "Backup node must be fully synced before promotion."
    echo "Run 'monad-status' to check details."
    exit 1
  fi
else
  warn "monad-status not installed, skipping sync check"
  echo "Install: curl -sSL $MF_BUCKET/scripts/monad-status.sh -o /usr/local/bin/monad-status && chmod +x /usr/local/bin/monad-status"
fi

# ── rpc security check ────────────────────────────────
step "RPC SECURITY CHECK"

if ss -ltn 2>/dev/null | grep -q ":8080"; then
  LISTEN_ADDR="$(ss -ltn | grep ':8080' | awk '{print $4}' | head -1)"
  if [[ "$LISTEN_ADDR" == *"0.0.0.0"* ]] || [[ "$LISTEN_ADDR" == *"*"* ]]; then
    warn "RPC port 8080 is publicly listening"
    echo "  Validators should not expose RPC publicly."
    echo "  Consider binding to localhost or adding a firewall rule."
    echo
  else
    ok "RPC not publicly exposed"
  fi
else
  ok "RPC port 8080 not active"
fi

# ── network detect ─────────────────────────────────────
NETWORK="$(grep '^network_name' "$NODE_TOML" | cut -d '"' -f2 || true)"
if [[ -z "$NETWORK" ]]; then
  die "Could not detect network_name from node.toml"
fi
ok "Network: $NETWORK"

echo
echo "This will promote this full node to validator."
echo "Make sure the original validator services are ${BOLD}STOPPED${RESET}."
echo
read -r -p "Continue? (y/N): " confirm
case "${confirm,,}" in y|yes) ;; *) die "Aborted.";; esac

# ── backup existing config ─────────────────────────────
step "BACKUP CURRENT CONFIG"

BACKUP_DIR="/opt/monad/backup/failover-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP_DIR"

for f in node.toml id-secp id-bls; do
  if [ -f "$CONFIG_DIR/$f" ]; then
    cp -a "$CONFIG_DIR/$f" "$BACKUP_DIR/"
  fi
done
ok "Config backed up to $BACKUP_DIR"

# ── download validator node.toml ───────────────────────
step "DOWNLOAD VALIDATOR node.toml"

echo "Downloading validator config template for $NETWORK..."
curl -fsSL -o "$NODE_TOML" "$MF_BUCKET/config/$NETWORK/latest/node.toml"
ok "Validator node.toml downloaded"

# ── beneficiary ───────────────────────────────────────
bar
echo "${BOLD}BENEFICIARY${RESET}"
echo "Enter the beneficiary address from the failed validator's node.toml"
read -r -p "beneficiary: " BENEFICIARY
if [[ -n "$BENEFICIARY" ]]; then
  sed -i "s|^beneficiary.*|beneficiary = \"$BENEFICIARY\"|" "$NODE_TOML"
  ok "Beneficiary set: $BENEFICIARY"
else
  warn "No beneficiary entered. Check node.toml manually."
fi

# ── seq_num ────────────────────────────────────────────
bar
echo "${BOLD}SEQ NUM${RESET}"
echo "Enter last seq_num from failed validator's node.toml (example: 0)"
read -r -p "seq_num: " LAST_SEQ
[[ "$LAST_SEQ" =~ ^[0-9]+$ ]] || die "Must be a number"
NEW_SEQ=$((LAST_SEQ + 1))
ok "New seq_num: $NEW_SEQ"

# ── key import ─────────────────────────────────────────
bar
echo "${BOLD}KEY IMPORT${RESET}"
echo "Paste IKM hex values. Input is hidden."
echo

read -r -s -p "SECP IKM_HEX: " SECP_IKM; echo
[ -n "$SECP_IKM" ] || die "Empty SECP IKM"

read -r -s -p "BLS  IKM_HEX: " BLS_IKM; echo
[ -n "$BLS_IKM" ] || die "Empty BLS IKM"

step "Importing SECP key"
monad-keystore import \
  --ikm "$SECP_IKM" \
  --keystore-path "$SECP_KEY" \
  --password "$KEYSTORE_PASSWORD"
ok "SECP key imported"

step "Importing BLS key"
monad-keystore import \
  --ikm "$BLS_IKM" \
  --keystore-path "$BLS_KEY" \
  --password "$KEYSTORE_PASSWORD"
ok "BLS key imported"

# ── verify keys ────────────────────────────────────────
bar
echo "${BOLD}VERIFY KEYS${RESET}"

SECP_PUB="$(monad-keystore recover \
  --password "$KEYSTORE_PASSWORD" \
  --keystore-path "$SECP_KEY" \
  --key-type secp 2>/dev/null | grep -i 'Secp public key' | awk '{print $NF}')"

BLS_PUB="$(monad-keystore recover \
  --password "$KEYSTORE_PASSWORD" \
  --keystore-path "$BLS_KEY" \
  --key-type bls 2>/dev/null | grep -i 'BLS public key' | awk '{print $NF}')"

[ -n "$SECP_PUB" ] || die "Could not recover SECP public key"
[ -n "$BLS_PUB" ]  || die "Could not recover BLS public key"

TS="$(date +%H:%M:%S)"
echo "[$TS] INFO  SECP Pubkey: ${SECP_PUB:0:46}..."
echo "[$TS] INFO  BLS  Pubkey: ${BLS_PUB:0:46}..."
echo
read -r -p "Do these match your validator keys? (y/N): " key_confirm
case "${key_confirm,,}" in y|yes) ;; *) die "Key mismatch. Aborting.";; esac
ok "Keys verified"

# ── ip detect ──────────────────────────────────────────
bar
echo "${BOLD}NODE ADDRESS${RESET}"

IP="$(curl -s4 ifconfig.me || true)"
[ -n "$IP" ] || die "Could not detect public IP"
echo "Detected IP: ${BOLD}$IP${RESET}"
read -r -p "Use this IP? (Y/n): " ip_confirm
case "${ip_confirm,,}" in
  n|no) read -r -p "Enter IP: " IP; [ -n "$IP" ] || die "Empty IP";;
esac
ok "Using: $IP"

# ── sign name record ──────────────────────────────────
step "SIGN NAME RECORD"

SIGN_OUT="$(monad-sign-name-record \
  --address "$IP:8000" \
  --node-config "$NODE_TOML" \
  --authenticated-udp-port 8001 \
  --self-record-seq-num "$NEW_SEQ" \
  --keystore-path "$SECP_KEY" \
  --password "$KEYSTORE_PASSWORD")"

SELF_ADDRESS="$(echo "$SIGN_OUT" | grep '^self_address ' | cut -d '"' -f2)"
SELF_SIG="$(echo "$SIGN_OUT" | grep '^self_name_record_sig ' | cut -d '"' -f2)"

[ -n "$SELF_ADDRESS" ] || die "Failed to parse self_address from name record output"
[ -n "$SELF_SIG" ]     || die "Failed to parse self_name_record_sig from output"
ok "Name record signed"

# ── patch node.toml ───────────────────────────────────
step "PATCH node.toml"

sed -i "s|^self_address.*|self_address = \"$SELF_ADDRESS\"|" "$NODE_TOML"
sed -i "s|^self_record_seq_num.*|self_record_seq_num = $NEW_SEQ|" "$NODE_TOML"
sed -i "s|^self_name_record_sig.*|self_name_record_sig = \"$SELF_SIG\"|" "$NODE_TOML"
ok "node.toml updated"

# ── summary ────────────────────────────────────────────
bar
echo "${BOLD}SUMMARY${RESET}"
echo "  Network:     $NETWORK"
echo "  IP:          $SELF_ADDRESS"
echo "  seq_num:     $NEW_SEQ"
echo "  Beneficiary: ${BENEFICIARY:-not set}"
echo "  SECP key:    ${SECP_PUB:0:20}..."
echo "  BLS  key:    ${BLS_PUB:0:20}..."
echo
read -r -p "Proceed with hard reset and start? (y/N): " final_confirm
case "${final_confirm,,}" in y|yes) ;; *) die "Aborted.";; esac

# ── stop services ──────────────────────────────────────
step "STOP SERVICES"
systemctl stop monad-bft monad-execution monad-rpc 2>/dev/null || true
ok "Services stopped"

# ── hard reset ─────────────────────────────────────────
step "HARD RESET"

echo "Resetting workspace..."
bash /opt/monad/scripts/reset-workspace.sh

echo "Restoring snapshot ($NETWORK)..."
curl -sSL "$MF_BUCKET/scripts/$NETWORK/restore-from-snapshot.sh" | bash

echo "Downloading forkpoint..."
curl -sSL "$MF_BUCKET/scripts/$NETWORK/download-forkpoint.sh" | bash

echo "Downloading validators.toml..."
curl -fsSL "$MF_BUCKET/validators/$NETWORK/validators.toml" -o "$VALIDATORS_FILE"
chown monad:monad "$VALIDATORS_FILE"

ok "Hard reset complete"

# ── start services ─────────────────────────────────────
step "START SERVICES"
systemctl start monad-bft monad-execution monad-rpc
ok "Services started"

# ── done ───────────────────────────────────────────────
echo
bar
ok "${BOLD}VALIDATOR PROMOTION COMPLETE${RESET}"
bar
echo
echo "Check status:"
echo "  systemctl status monad-bft monad-execution monad-rpc --no-pager -l"
echo "  journalctl -fu monad-bft"
if command -v monad-status >/dev/null 2>&1; then
  echo "  monad-status"
fi
echo
