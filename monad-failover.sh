#!/usr/bin/env bash
#
# monad-failover v2 — Monad validator mobility toolkit
#
# Modes:
#   promote           Promote an already-synced full node to validator
#   prepare-standby   Sync a box as a full node under temporary keys
#   restore-fullnode  Restore a box to its original full-node identity
#
# Usage:
#   ./monad-failover.sh promote          [--peer-host user@host] [--resume]
#   ./monad-failover.sh prepare-standby  [--snapshot-reset] [--resume]
#   ./monad-failover.sh restore-fullnode [--resume]

set -euo pipefail

VERSION="3.0.0"

# ── paths ──────────────────────────────────────────────────
MONAD_HOME="/home/monad"
CONFIG_DIR="$MONAD_HOME/monad-bft/config"
NODE_TOML="$CONFIG_DIR/node.toml"
ENV_FILE="$MONAD_HOME/.env"
SECP_KEY="$CONFIG_DIR/id-secp"
BLS_KEY="$CONFIG_DIR/id-bls"
SECP_KEY_NEW="$CONFIG_DIR/id-secp.new"
BLS_KEY_NEW="$CONFIG_DIR/id-bls.new"
MF_BUCKET="https://bucket.monadinfra.com"
BACKUP_ROOT="/opt/monad/backup"
VALIDATORS_DIR="$CONFIG_DIR/validators"
VALIDATORS_TOML="$VALIDATORS_DIR/validators.toml"
BURN_ADDRESS="0x0000000000000000000000000000000000000000"
MONAD_SERVICES=(monad-bft monad-execution monad-rpc)

# ── ui helpers ─────────────────────────────────────────────
BOLD=$'\033[1m' DIM=$'\033[2m'
RED=$'\033[31m' GREEN=$'\033[32m' YELLOW=$'\033[33m' CYAN=$'\033[36m'
RESET=$'\033[0m'

header() {
  local mode_label="${1:-}"
  echo
  echo "╔══════════════════════════════════════════════╗"
  echo "║        MONAD VALIDATOR FAILOVER TOOL         ║"
  echo "╚══════════════════════════════════════════════╝"
  echo
  [[ -n "$mode_label" ]] && echo "  Mode:     ${BOLD}${mode_label}${RESET}"
  echo "  Hostname: ${BOLD}$(hostname)${RESET}"
  echo "  Date:     $(date '+%Y-%m-%d %H:%M:%S %Z')"
  echo "  Version:  $VERSION"
  echo
}

bar()      { echo "${DIM}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"; }
step()     { echo; echo "${CYAN}▶${RESET} ${BOLD}$*${RESET}"; }
ok()       { echo "${GREEN}✔${RESET} $*"; }
warn()     { echo "${YELLOW}⚠${RESET} $*"; }
die()      { echo "${RED}✗${RESET} $*" >&2; exit 1; }
need_cmd() { command -v "$1" >/dev/null 2>&1 || die "Missing command: $1"; }

confirm_yn() {
  local prompt="$1"
  read -r -p "$prompt (y/N): " ans
  case "${ans,,}" in y|yes) return 0 ;; *) return 1 ;; esac
}

# ── state / resume ─────────────────────────────────────────
STATE_DIR=""
STATE_FILE=""

init_state() {
  STATE_DIR="$MONAD_HOME/.monad-failover/$MODE"
  STATE_FILE="$STATE_DIR/state"
  mkdir -p "$STATE_DIR"
}

save_state() {
  if grep -q "^${1}=" "$STATE_FILE" 2>/dev/null; then
    sed -i "s|^${1}=.*|${1}=${2}|" "$STATE_FILE"
  else
    echo "${1}=${2}" >> "$STATE_FILE"
  fi
}

load_state() {
  grep "^${1}=" "$STATE_FILE" 2>/dev/null | cut -d '=' -f2- || true
}

clear_state() { rm -f "$STATE_FILE"; }

completed_step() {
  local c; c="$(load_state "last_step")"
  [[ -n "$c" ]] && [[ "$c" -ge "$1" ]]
}

# ── common guards ──────────────────────────────────────────
check_sync() {
  step "NODE SYNC CHECK"
  if command -v monad-status >/dev/null 2>&1; then
    local out status diff
    out="$(monad-status 2>/dev/null || true)"
    status="$(echo "$out" | grep -m1 'status:' | awk '{print $2}' || true)"
    diff="$(echo "$out" | grep -m1 'blockDifference:' | awk '{print $2}' || true)"
    if [[ "$status" == "in-sync" ]]; then
      ok "Node: in-sync (block difference: ${diff:-0})"
    else
      die "Node is ${status:-unknown}. Must be fully synced before promotion."
    fi
  else
    warn "monad-status not installed — cannot verify sync"
    confirm_yn "  Continue without sync check?" || die "Aborted."
  fi
}

check_rpc() {
  step "RPC SECURITY CHECK"
  if ss -ltn 2>/dev/null | grep -q ":8080"; then
    local addr
    addr="$(ss -ltn | grep ':8080' | awk '{print $4}' | head -1)"
    if [[ "$addr" == *"0.0.0.0"* ]] || [[ "$addr" == *"*"* ]]; then
      warn "RPC port 8080 is publicly listening"
      echo "  Validators should not expose RPC publicly."
      echo "  Consider binding to localhost or adding a firewall rule."
    else
      ok "RPC not publicly exposed"
    fi
  else
    ok "RPC port 8080 not active"
  fi
}

detect_network() {
  local toml="${1:-$NODE_TOML}"
  NETWORK="$(grep '^network_name' "$toml" 2>/dev/null | cut -d '"' -f2 || true)"
  if [[ -z "$NETWORK" ]]; then
    read -r -p "Could not detect network. Enter (mainnet/testnet): " NETWORK
    [[ "$NETWORK" == "mainnet" || "$NETWORK" == "testnet" ]] || die "Invalid network"
  fi
  ok "Network: $NETWORK"
}

run_location_guard() {
  local mode_desc="$1"
  bar
  warn "This will ${BOLD}${mode_desc}${RESET}."
  echo "  Hostname: ${BOLD}$(hostname)${RESET}"

  local ip
  ip="$(curl -s4 --connect-timeout 5 ifconfig.me || true)"
  [[ -n "$ip" ]] && echo "  Public IP: ${BOLD}${ip}${RESET}"

  if [[ -f "$SECP_KEY" ]] && [[ -n "${KEYSTORE_PASSWORD:-}" ]]; then
    local cur_pub
    cur_pub="$(monad-keystore recover --password "$KEYSTORE_PASSWORD" \
      --keystore-path "$SECP_KEY" --key-type secp 2>/dev/null \
      | grep -i 'Secp public key' | awk '{print $NF}' || true)"
    [[ -n "$cur_pub" ]] && echo "  Current SECP: ${cur_pub:0:24}..."
  fi

  echo
  confirm_yn "Is this the correct target host?" || die "Aborted."
}

sanitize_placeholders() {
  local toml="$1"
  if grep -qE '^self_address\s*=\s*"<' "$toml" 2>/dev/null; then
    sed -i 's|^self_address.*|self_address = "0.0.0.0:8000"|' "$toml"
  fi
  if grep -qE '^self_record_seq_num\s*=\s*[^0-9]' "$toml" 2>/dev/null; then
    sed -i 's|^self_record_seq_num.*|self_record_seq_num = 0|' "$toml"
  fi
  local zero_sig
  zero_sig="$(printf '0%.0s' $(seq 1 130))"
  if grep -qE '^self_name_record_sig\s*=\s*"(<|[^0-9a-fA-F])' "$toml" 2>/dev/null; then
    sed -i "s|^self_name_record_sig.*|self_name_record_sig = \"$zero_sig\"|" "$toml"
  fi
  ok "Placeholders sanitized"
}

fix_ownership() {
  chown -R monad:monad "$CONFIG_DIR" 2>/dev/null || true
  chown monad:monad "$ENV_FILE" 2>/dev/null || true
  chmod 600 "$SECP_KEY" "$BLS_KEY" 2>/dev/null || true
}

backup_config() {
  step "BACKUP CURRENT CONFIG"
  local ts
  ts="$(date +%Y%m%d-%H%M%S)"
  BACKUP_DIR="$BACKUP_ROOT/failover-${MODE}-${ts}"
  mkdir -p "$BACKUP_DIR"

  for f in node.toml id-secp id-bls; do
    [[ -f "$CONFIG_DIR/$f" ]] && cp -a "$CONFIG_DIR/$f" "$BACKUP_DIR/"
  done
  [[ -f "$MONAD_HOME/pubkey-secp-bls" ]] && cp -a "$MONAD_HOME/pubkey-secp-bls" "$BACKUP_DIR/"
  [[ -f "$ENV_FILE" ]] && cp -a "$ENV_FILE" "$BACKUP_DIR/"
  for f in secp-backup bls-backup; do
    [[ -f "$BACKUP_ROOT/$f" ]] && cp -a "$BACKUP_ROOT/$f" "$BACKUP_DIR/"
  done

  if [[ -n "${KEYSTORE_PASSWORD:-}" ]]; then
    printf '%s' "$KEYSTORE_PASSWORD" > "$BACKUP_DIR/keystore-password-backup"
    chmod 600 "$BACKUP_DIR/keystore-password-backup"
  fi

  ok "Config backed up to $BACKUP_DIR"
  save_state "backup_dir" "$BACKUP_DIR"
}

check_colocated_services() {
  local colocated=()
  for svc in axelard tofnd vald nginx; do
    systemctl is-active --quiet "$svc" 2>/dev/null && colocated+=("$svc")
  done
  if [[ ${#colocated[@]} -gt 0 ]]; then
    warn "Co-located services: ${colocated[*]}"
    echo "  This tool only manages: ${MONAD_SERVICES[*]}"
    echo "  It will NOT touch the services above."
  fi
}

stop_monad_services() {
  step "STOP MONAD SERVICES"
  systemctl stop "${MONAD_SERVICES[@]}" 2>/dev/null || true
  sleep 1
  local still=false
  for svc in "${MONAD_SERVICES[@]}"; do
    if systemctl is-active --quiet "$svc" 2>/dev/null; then
      still=true; warn "$svc still running"
    fi
  done
  $still && die "Could not stop all services"
  ok "Services stopped"
}

start_monad_services() {
  step "START MONAD SERVICES"
  systemctl start "${MONAD_SERVICES[@]}"
  ok "Services started"
}

verify_config_flags() {
  step "VERIFY CONFIG FLAGS"
  local missing=""
  grep -q '^enable_publisher = true' "$NODE_TOML" || missing="${missing} enable_publisher"
  grep -q '^enable_client = true' "$NODE_TOML"    || missing="${missing} enable_client"
  grep -q '^expand_to_group = true' "$NODE_TOML"  || missing="${missing} expand_to_group"

  if [[ -n "$missing" ]]; then
    warn "Flags not set:${missing}"
    echo "  The official migration docs require these to be true."
  else
    ok "enable_publisher, enable_client, expand_to_group all set"
  fi
}

set_toml_value() {
  local file="$1" key="$2" value="$3" section="${4:-}"

  if grep -qE "^[[:space:]]*${key}[[:space:]]*=" "$file" 2>/dev/null; then
    sed -i "s|^[[:space:]]*${key}[[:space:]]*=.*|${key} = ${value}|" "$file"
    return
  fi

  if [[ -z "$section" ]]; then
    die "Key '$key' not found in $file and no section given — refusing to append blindly."
  fi
  grep -qF "[$section]" "$file" \
    || die "Section [$section] not found in $file — cannot place '$key'."
  sed -i "\\|^\\[$section\\]|a ${key} = ${value}" "$file"
  grep -qE "^[[:space:]]*${key}[[:space:]]*=" "$file" \
    || die "Failed to insert '$key' under [$section]."
}

sign_and_patch() {
  local toml="$1" ip="$2" seq="$3" keypath="$4" password="$5"
  local use_node_config="${6:-true}"

  sanitize_placeholders "$toml"

  step "SIGN NAME RECORD (seq $seq)"
  local sign_out sign_args=(
    --address "$ip:8000"
    --authenticated-udp-port 8001
    --self-record-seq-num "$seq"
    --keystore-path "$keypath"
    --password "$password"
  )
  [[ "$use_node_config" == "true" ]] && sign_args+=(--node-config "$toml")

  if ! sign_out="$(monad-sign-name-record "${sign_args[@]}")"; then
    die "monad-sign-name-record failed"
  fi

  SELF_ADDRESS="$(echo "$sign_out" | grep '^self_address ' | cut -d '"' -f2 || true)"
  SELF_SIG="$(echo "$sign_out" | grep '^self_name_record_sig ' | cut -d '"' -f2 || true)"

  [[ -n "$SELF_ADDRESS" ]] || die "Failed to parse self_address from signer output"
  [[ -n "$SELF_SIG" ]]     || die "Failed to parse self_name_record_sig from signer output"
  ok "Name record signed"

  step "PATCH node.toml"
  sed -i "s|^self_address.*|self_address = \"$SELF_ADDRESS\"|" "$toml"
  sed -i "s|^self_record_seq_num.*|self_record_seq_num = $seq|" "$toml"
  sed -i "s|^self_name_record_sig.*|self_name_record_sig = \"$SELF_SIG\"|" "$toml"

  grep -q "^self_address = \"$SELF_ADDRESS\"" "$toml"  || die "Failed to write self_address"
  grep -q "^self_record_seq_num = $seq" "$toml"         || die "Failed to write self_record_seq_num"
  fix_ownership
  ok "node.toml patched and verified"
}

post_verify() {
  step "POST-ACTION VERIFICATION"
  sleep 3
  if command -v monad-status >/dev/null 2>&1; then
    local out status
    out="$(monad-status 2>/dev/null || true)"
    status="$(echo "$out" | grep -m1 'status:' | awk '{print $2}' || true)"
    if [[ "$status" == "in-sync" ]]; then
      ok "Node is in-sync"
    else
      warn "Status: ${status:-starting...} (may take a moment)"
    fi
  fi
}

detect_ip() {
  IP="$(curl -s4 --connect-timeout 10 ifconfig.me || true)"
  [[ -n "$IP" ]] || die "Could not detect public IP"
  ok "Public IP: $IP"
}

# ══════════════════════════════════════════════════════════
# MODE: promote
# ══════════════════════════════════════════════════════════
mode_promote() {
  header "promote"

  need_cmd curl; need_cmd systemctl; need_cmd sed
  need_cmd monad-keystore; need_cmd monad-sign-name-record
  [[ -f "$NODE_TOML" ]] || die "node.toml not found: $NODE_TOML"
  [[ -f "$ENV_FILE" ]]  || die ".env not found: $ENV_FILE"

  # shellcheck disable=SC1090
  source "$ENV_FILE"
  [[ -n "${KEYSTORE_PASSWORD:-}" ]] || die "KEYSTORE_PASSWORD not set in $ENV_FILE"

  # ── resume restore ──
  if $RESUME; then
    local last; last="$(load_state "last_step")"
    if [[ -z "$last" ]]; then
      warn "No previous promote run found. Starting fresh."; RESUME=false
    else
      ok "Resuming from step $((last + 1))"
      NETWORK="$(load_state "network")"
      NEW_SEQ="$(load_state "new_seq")"
      SECP_PUB="$(load_state "secp_pub")"
      BLS_PUB="$(load_state "bls_pub")"
      IP="$(load_state "ip")"
      SELF_ADDRESS="$(load_state "self_address")"
      SELF_SIG="$(load_state "self_sig")"
      BENEFICIARY="$(load_state "beneficiary")"
      BACKUP_DIR="$(load_state "backup_dir")"
    fi
  fi

  # ── 1. Sync + RPC + colocated ──
  if ! $RESUME || ! completed_step 1; then
    check_sync
    check_rpc
    check_colocated_services
    save_state "last_step" "1"
  fi

  # ── 2. Network + location guard ──
  if ! $RESUME || ! completed_step 2; then
    step "NETWORK & HOST CONFIRMATION"
    detect_network
    run_location_guard "promote this full node to validator"
    save_state "network" "$NETWORK"
    save_state "last_step" "2"
  fi

  # ── 3. Backup ──
  if ! $RESUME || ! completed_step 3; then
    backup_config
    save_state "last_step" "3"
  fi

  # ── 4. Import validator keys (to staging paths — live keys untouched) ──
  if ! $RESUME || ! completed_step 4; then
    rm -f "$SECP_KEY_NEW" "$BLS_KEY_NEW"

    bar
    echo "${BOLD}KEY IMPORT${RESET}"
    echo "Paste the validator IKM hex values. Input is hidden."
    echo "  Keys are imported to staging files (id-secp.new / id-bls.new)."
    echo "  Live keys remain untouched until cutover."
    echo

    read -r -s -p "SECP IKM_HEX: " SECP_IKM; echo
    [[ -n "$SECP_IKM" ]] || die "Empty SECP IKM"
    SECP_IKM="${SECP_IKM#0x}"
    [[ "$SECP_IKM" =~ ^[0-9a-fA-F]{64}$ ]] \
      || die "SECP IKM must be 64 hex characters"

    read -r -s -p "BLS  IKM_HEX: " BLS_IKM; echo
    [[ -n "$BLS_IKM" ]] || die "Empty BLS IKM"
    BLS_IKM="${BLS_IKM#0x}"
    [[ "$BLS_IKM" =~ ^[0-9a-fA-F]{64}$ ]] \
      || die "BLS IKM must be 64 hex characters"

    step "Importing SECP key (staging)"
    monad-keystore import \
      --ikm "$SECP_IKM" \
      --keystore-path "$SECP_KEY_NEW" \
      --password "$KEYSTORE_PASSWORD"
    ok "SECP key imported to id-secp.new"

    step "Importing BLS key (staging)"
    monad-keystore import \
      --ikm "$BLS_IKM" \
      --keystore-path "$BLS_KEY_NEW" \
      --password "$KEYSTORE_PASSWORD"
    ok "BLS key imported to id-bls.new"

    chown monad:monad "$SECP_KEY_NEW" "$BLS_KEY_NEW" 2>/dev/null || true
    chmod 600 "$SECP_KEY_NEW" "$BLS_KEY_NEW" 2>/dev/null || true
    SECP_IKM="" BLS_IKM=""

    bar
    echo "${BOLD}VERIFY KEYS${RESET}"

    SECP_PUB="$(monad-keystore recover \
      --password "$KEYSTORE_PASSWORD" \
      --keystore-path "$SECP_KEY_NEW" \
      --key-type secp 2>/dev/null | grep -i 'Secp public key' | awk '{print $NF}' || true)"
    BLS_PUB="$(monad-keystore recover \
      --password "$KEYSTORE_PASSWORD" \
      --keystore-path "$BLS_KEY_NEW" \
      --key-type bls 2>/dev/null | grep -i 'BLS public key' | awk '{print $NF}' || true)"

    [[ -n "$SECP_PUB" ]] || die "Could not recover SECP public key"
    [[ -n "$BLS_PUB" ]]  || die "Could not recover BLS public key"

    echo "  SECP: ${SECP_PUB:0:46}..."
    echo "  BLS:  ${BLS_PUB:0:46}..."
    echo
    confirm_yn "Do these match your validator keys?" || die "Key mismatch — aborting."
    ok "Keys verified"

    save_state "secp_pub" "$SECP_PUB"
    save_state "bls_pub" "$BLS_PUB"
    save_state "last_step" "4"
  fi

  # ── 5. Beneficiary + seq + flags ──
  if ! $RESUME || ! completed_step 5; then
    bar
    echo "${BOLD}BENEFICIARY${RESET}"
    echo "Enter beneficiary address from the failed validator's node.toml"
    read -r -p "beneficiary: " BENEFICIARY
    if [[ -n "$BENEFICIARY" ]]; then
      set_toml_value "$NODE_TOML" "beneficiary" "\"$BENEFICIARY\""
      ok "Beneficiary: $BENEFICIARY"
    else
      warn "No beneficiary entered — check node.toml manually after promotion."
    fi

    bar
    echo "${BOLD}SEQ NUM${RESET}"
    echo "Enter last self_record_seq_num from the failed validator's node.toml"
    read -r -p "seq_num: " LAST_SEQ
    [[ "$LAST_SEQ" =~ ^[0-9]+$ ]] || die "Must be a number"
    NEW_SEQ=$((LAST_SEQ + 1))
    ok "New seq_num: $NEW_SEQ"

    set_toml_value "$NODE_TOML" "enable_publisher" "true"  "fullnode_raptorcast"
    set_toml_value "$NODE_TOML" "enable_client"    "true"  "fullnode_raptorcast"
    set_toml_value "$NODE_TOML" "expand_to_group"  "true"  "statesync"
    verify_config_flags

    save_state "beneficiary" "${BENEFICIARY:-}"
    save_state "new_seq" "$NEW_SEQ"
    save_state "last_step" "5"
  fi

  # ── 6. Sign name record + patch (using staging key) ──
  if ! $RESUME || ! completed_step 6; then
    detect_ip
    sign_and_patch "$NODE_TOML" "$IP" "$NEW_SEQ" "$SECP_KEY_NEW" "$KEYSTORE_PASSWORD"

    save_state "ip" "$IP"
    save_state "self_address" "$SELF_ADDRESS"
    save_state "self_sig" "$SELF_SIG"
    save_state "last_step" "6"
  fi

  # ── 7. Confirm old validator stopped + cutover ──
  if ! $RESUME || ! completed_step 7; then
    bar
    echo "${BOLD}PROMOTION SUMMARY${RESET}"
    echo
    echo "  Hostname:    $(hostname)"
    echo "  Network:     $NETWORK"
    echo "  Address:     $SELF_ADDRESS"
    echo "  seq_num:     $NEW_SEQ"
    echo "  Beneficiary: ${BENEFICIARY:-not set}"
    echo "  SECP key:    ${SECP_PUB:0:24}..."
    echo "  BLS  key:    ${BLS_PUB:0:24}..."
    echo

    if [[ -n "$PEER_HOST" ]]; then
      step "CHECK PEER HOST: $PEER_HOST"
      local peer_active=false
      for svc in monad-bft monad-execution; do
        if ssh -o ConnectTimeout=10 "$PEER_HOST" \
          "systemctl is-active --quiet $svc" 2>/dev/null; then
          peer_active=true
        fi
      done
      if $peer_active; then
        die "Monad services still running on $PEER_HOST. Stop them first:" \
          "  ssh $PEER_HOST 'systemctl stop ${MONAD_SERVICES[*]}'" \
          "then re-run with --resume."
      else
        ok "Peer services already stopped"
      fi
    else
      warn "The original validator must be STOPPED before proceeding."
      echo "  On the old validator, run:"
      echo "    systemctl stop monad-bft monad-execution monad-rpc"
      echo
      confirm_yn "Original validator stopped and verified?" || die "Aborted."
    fi

    bar
    warn "${BOLD}POINT OF NO RETURN${RESET}"
    echo "  The next step stops services, swaps in the validator keys, and starts."
    echo "  After this the old validator MUST NOT be restarted with the same keys."
    echo
    confirm_yn "Proceed with cutover?" || die "Aborted."

    step "CUTOVER"
    stop_monad_services
    mv "$SECP_KEY_NEW" "$SECP_KEY"
    mv "$BLS_KEY_NEW" "$BLS_KEY"
    fix_ownership
    systemctl enable "${MONAD_SERVICES[@]}" 2>/dev/null || true
    start_monad_services

    save_state "last_step" "7"
  fi

  # ── 8. Verify ──
  if ! $RESUME || ! completed_step 8; then
    post_verify
    save_state "last_step" "8"
  fi

  # ── done ──
  rm -f "$SECP_KEY_NEW" "$BLS_KEY_NEW" 2>/dev/null || true
  clear_state
  echo
  bar
  ok "${BOLD}VALIDATOR PROMOTION COMPLETE${RESET}"
  bar

  echo
  echo "${BOLD}NODE STATUS${RESET}"
  echo "  systemctl status monad-bft monad-execution monad-rpc --no-pager -l"
  echo "  journalctl -fu monad-bft"
  command -v monad-status >/dev/null 2>&1 && echo "  monad-status"

  echo
  echo "${BOLD}VALIDATOR EVENTS${RESET}"
  echo "  journalctl -u monad-ledger-tail -o cat -f | grep -i \"${SECP_PUB}\""
  if [[ "$NETWORK" == "mainnet" ]]; then
    echo "  https://monad.hoodscan.io/validator/${SECP_PUB}"
  else
    echo "  https://testnet.monad.hoodscan.io/validator/${SECP_PUB}?tab=Performance"
  fi

  echo
  warn "If you have downstream full nodes, update this validator's"
  echo "  name record in their node.toml to maintain connectivity."
  echo
}

# ══════════════════════════════════════════════════════════
# MODE: prepare-standby
# ══════════════════════════════════════════════════════════
mode_prepare_standby() {
  header "prepare-standby"
  rm -f "$SECP_KEY_NEW" "$BLS_KEY_NEW" 2>/dev/null || true

  need_cmd curl; need_cmd systemctl; need_cmd sed; need_cmd openssl
  need_cmd monad-keystore; need_cmd monad-sign-name-record
  [[ -f "$ENV_FILE" ]] || die ".env not found: $ENV_FILE"
  $SNAPSHOT_RESET && need_cmd aria2c

  # shellcheck disable=SC1090
  source "$ENV_FILE"
  [[ -n "${KEYSTORE_PASSWORD:-}" ]] || die "KEYSTORE_PASSWORD not set in $ENV_FILE"

  if $RESUME; then
    local last; last="$(load_state "last_step")"
    if [[ -z "$last" ]]; then
      warn "No previous prepare-standby run found. Starting fresh."; RESUME=false
    else
      ok "Resuming from step $((last + 1))"
      NETWORK="$(load_state "network")"
      SECP_PUB="$(load_state "secp_pub")"
      BLS_PUB="$(load_state "bls_pub")"
      BACKUP_DIR="$(load_state "backup_dir")"
    fi
  fi

  # ── 1. Location guard + active validator check + backup ──
  if ! $RESUME || ! completed_step 1; then
    run_location_guard "prepare this box as a standby full node"

    if [[ -f "$SECP_KEY" ]] && [[ -n "${KEYSTORE_PASSWORD:-}" ]] && [[ -f "$VALIDATORS_TOML" ]]; then
      local cur_pub
      cur_pub="$(monad-keystore recover --password "$KEYSTORE_PASSWORD" \
        --keystore-path "$SECP_KEY" --key-type secp 2>/dev/null \
        | grep -i 'Secp public key' | awk '{print $NF}' || true)"
      if [[ -n "$cur_pub" ]] && grep -q "$cur_pub" "$VALIDATORS_TOML" 2>/dev/null; then
        die "This box holds an ACTIVE VALIDATOR key (${cur_pub:0:24}...) listed in validators.toml. Running prepare-standby here would overwrite it with temp keys. Aborting."
      fi
    fi

    check_colocated_services

    if [[ -f "$NODE_TOML" ]] || [[ -f "$SECP_KEY" ]] || [[ -f "$BLS_KEY" ]]; then
      # Detect network from existing config before overwriting
      if [[ -f "$NODE_TOML" ]]; then
        detect_network "$NODE_TOML"
        save_state "network" "$NETWORK"
      fi
      backup_config
    else
      ok "No existing config to back up"
      BACKUP_DIR=""
      save_state "backup_dir" ""
    fi
    save_state "last_step" "1"
  fi

  # ── 2. Generate temporary keys ──
  if ! $RESUME || ! completed_step 2; then
    step "GENERATE TEMPORARY KEYS"
    echo "  These are throwaway keys for syncing. NOT your validator keys."

    local temp_ikm
    temp_ikm="$(openssl rand -hex 32)"
    monad-keystore import \
      --ikm "$temp_ikm" \
      --keystore-path "$SECP_KEY" \
      --password "$KEYSTORE_PASSWORD"
    ok "Temporary SECP key generated"

    temp_ikm="$(openssl rand -hex 32)"
    monad-keystore import \
      --ikm "$temp_ikm" \
      --keystore-path "$BLS_KEY" \
      --password "$KEYSTORE_PASSWORD"
    ok "Temporary BLS key generated"
    temp_ikm=""
    fix_ownership

    SECP_PUB="$(monad-keystore recover \
      --password "$KEYSTORE_PASSWORD" \
      --keystore-path "$SECP_KEY" \
      --key-type secp 2>/dev/null | grep -i 'Secp public key' | awk '{print $NF}' || true)"
    BLS_PUB="$(monad-keystore recover \
      --password "$KEYSTORE_PASSWORD" \
      --keystore-path "$BLS_KEY" \
      --key-type bls 2>/dev/null | grep -i 'BLS public key' | awk '{print $NF}' || true)"

    [[ -n "$SECP_PUB" ]] || die "Could not recover temp SECP key"
    [[ -n "$BLS_PUB" ]]  || die "Could not recover temp BLS key"

    echo "  Temp SECP: ${SECP_PUB:0:46}..."
    echo "  Temp BLS:  ${BLS_PUB:0:46}..."

    save_state "secp_pub" "$SECP_PUB"
    save_state "bls_pub" "$BLS_PUB"
    save_state "last_step" "2"
  fi

  # ── 3. Download full-node template + configure ──
  if ! $RESUME || ! completed_step 3; then
    step "DOWNLOAD FULL-NODE node.toml"

    if [[ -z "${NETWORK:-}" ]]; then
      if [[ -f "$NODE_TOML" ]]; then
        detect_network "$NODE_TOML" 2>/dev/null || true
      fi
      if [[ -z "${NETWORK:-}" ]]; then
        read -r -p "  Network (mainnet/testnet): " NETWORK
        [[ "$NETWORK" == "mainnet" || "$NETWORK" == "testnet" ]] || die "Invalid network"
      fi
      save_state "network" "$NETWORK"
    fi

    curl -fsSL -o "$NODE_TOML" "$MF_BUCKET/config/$NETWORK/latest/full-node-node.toml"
    ok "Full-node node.toml downloaded"

    set_toml_value "$NODE_TOML" "beneficiary" "\"$BURN_ADDRESS\""

    local node_name="standby-$(hostname | cut -c1-16)-$(date +%s | tail -c 5)"
    set_toml_value "$NODE_TOML" "node_name" "\"$node_name\""
    ok "node_name: $node_name"

    set_toml_value "$NODE_TOML" "enable_client"    "true"  "fullnode_raptorcast"
    set_toml_value "$NODE_TOML" "expand_to_group"  "true"  "statesync"

    save_state "last_step" "3"
  fi

  # ── 4. Check .env ──
  if ! $RESUME || ! completed_step 4; then
    step "CHECK .env CONFIGURATION"
    local env_ok=true
    grep -q 'REMOTE_VALIDATORS_URL' "$ENV_FILE" 2>/dev/null || { warn "REMOTE_VALIDATORS_URL not set in .env"; env_ok=false; }
    grep -q 'REMOTE_FORKPOINT_URL' "$ENV_FILE" 2>/dev/null  || { warn "REMOTE_FORKPOINT_URL not set in .env"; env_ok=false; }

    if ! $env_ok; then
      echo "  These are needed for the full node to sync."
      confirm_yn "Continue anyway?" || die "Aborted."
    else
      ok ".env configuration OK"
    fi
    save_state "last_step" "4"
  fi

  # ── 5. Sanitize + sign + patch ──
  if ! $RESUME || ! completed_step 5; then
    detect_ip
    sign_and_patch "$NODE_TOML" "$IP" "1" "$SECP_KEY" "$KEYSTORE_PASSWORD" "false"
    save_state "last_step" "5"
  fi

  # ── 6. Optional snapshot reset ──
  if ! $RESUME || ! completed_step 6; then
    if $SNAPSHOT_RESET; then
      step "SNAPSHOT RESET"
      stop_monad_services

      warn "Running workspace reset..."
      bash /opt/monad/scripts/reset-workspace.sh

      step "RESTORE FROM SNAPSHOT"
      curl -sSL "$MF_BUCKET/scripts/$NETWORK/restore-from-snapshot.sh" | bash
      ok "Snapshot restored"

      step "DOWNLOAD VALIDATORS + FORKPOINT"
      mkdir -p "$VALIDATORS_DIR"
      curl -fsSL -o "$VALIDATORS_TOML" "$MF_BUCKET/validators/$NETWORK/validators.toml"
      chown monad:monad "$VALIDATORS_TOML" 2>/dev/null || true
      curl -sSL "$MF_BUCKET/scripts/$NETWORK/download-forkpoint.sh" | bash
      ok "Config files downloaded"
    else
      ok "No snapshot reset (statesync will handle catch-up)"
    fi
    save_state "last_step" "6"
  fi

  # ── 7. Start services ──
  if ! $RESUME || ! completed_step 7; then
    systemctl enable "${MONAD_SERVICES[@]}" 2>/dev/null || true
    fix_ownership
    start_monad_services
    save_state "last_step" "7"
  fi

  # ── 8. Verify ──
  if ! $RESUME || ! completed_step 8; then
    post_verify
    save_state "last_step" "8"
  fi

  clear_state
  echo
  bar
  ok "${BOLD}STANDBY NODE CONFIGURED${RESET}"
  bar

  echo
  echo "  The node is syncing as a full node under temporary keys."
  echo "  Wait until monad-status shows ${BOLD}in-sync${RESET}, then run:"
  echo
  echo "    ${BOLD}./monad-failover.sh promote${RESET}"
  echo
  echo "  to swap in the validator keys and go live."
  echo
  echo "${BOLD}MONITOR${RESET}"
  echo "  monad-status"
  echo "  journalctl -fu monad-bft"
  echo
}

# ══════════════════════════════════════════════════════════
# MODE: restore-fullnode
# ══════════════════════════════════════════════════════════
mode_restore_fullnode() {
  header "restore-fullnode"
  rm -f "$SECP_KEY_NEW" "$BLS_KEY_NEW" 2>/dev/null || true

  need_cmd curl; need_cmd systemctl; need_cmd sed
  need_cmd monad-keystore; need_cmd monad-sign-name-record
  [[ -f "$ENV_FILE" ]] || die ".env not found: $ENV_FILE"

  if $RESUME; then
    local last; last="$(load_state "last_step")"
    if [[ -z "$last" ]]; then
      warn "No previous restore-fullnode run found. Starting fresh."; RESUME=false
    else
      ok "Resuming from step $((last + 1))"
      BACKUP_DIR="$(load_state "backup_dir")"
      SECP_PUB="$(load_state "secp_pub")"
      BLS_PUB="$(load_state "bls_pub")"
      NETWORK="$(load_state "network")"
      # shellcheck disable=SC1090
      source "$ENV_FILE"
    fi
  fi

  # ── 1. Verify preconditions + select backup ──
  if ! $RESUME || ! completed_step 1; then
    step "VERIFY PRECONDITIONS"

    for svc in "${MONAD_SERVICES[@]}"; do
      if systemctl is-active --quiet "$svc" 2>/dev/null; then
        die "$svc is still running. Stop services first: systemctl stop ${MONAD_SERVICES[*]}"
      fi
    done
    ok "Monad services are stopped"

    # shellcheck disable=SC1090
    source "$ENV_FILE"
    [[ -n "${KEYSTORE_PASSWORD:-}" ]] || die "KEYSTORE_PASSWORD not set in $ENV_FILE"

    run_location_guard "restore this box to its original full-node identity"
    check_colocated_services

    step "SELECT BACKUP"
    if [[ ! -d "$BACKUP_ROOT" ]]; then
      die "Backup directory not found: $BACKUP_ROOT"
    fi

    local -a backups=()
    while IFS= read -r d; do
      backups+=("$d")
    done < <(find "$BACKUP_ROOT" -maxdepth 1 -type d -name 'failover-*' | sort -r | head -10)

    if [[ ${#backups[@]} -eq 0 ]]; then
      die "No backups found in $BACKUP_ROOT"
    fi

    echo "  Available backups:"
    local i
    for i in "${!backups[@]}"; do
      local bdir="${backups[$i]}"
      local label="$(basename "$bdir")"
      local has_keys=""
      [[ -f "$bdir/id-secp" ]] && has_keys="keys:yes" || has_keys="keys:no"
      echo "    $((i + 1)). $label  ($has_keys)"
    done

    echo
    read -r -p "Select backup (1-${#backups[@]}): " sel
    [[ "$sel" =~ ^[0-9]+$ ]] && [[ "$sel" -ge 1 ]] && [[ "$sel" -le ${#backups[@]} ]] \
      || die "Invalid selection"
    BACKUP_DIR="${backups[$((sel - 1))]}"
    ok "Selected: $(basename "$BACKUP_DIR")"

    [[ -f "$BACKUP_DIR/id-secp" ]]  || die "Missing id-secp in backup"
    [[ -f "$BACKUP_DIR/id-bls" ]]   || die "Missing id-bls in backup"
    [[ -f "$BACKUP_DIR/node.toml" ]] || die "Missing node.toml in backup"
    ok "Backup contents verified"

    save_state "backup_dir" "$BACKUP_DIR"
    save_state "last_step" "1"
  fi

  # ── 2. Restore keys + config ──
  if ! $RESUME || ! completed_step 2; then
    step "RESTORE KEYS AND CONFIG"
    cp -a "$BACKUP_DIR/id-secp" "$SECP_KEY"
    cp -a "$BACKUP_DIR/id-bls" "$BLS_KEY"
    cp -a "$BACKUP_DIR/node.toml" "$NODE_TOML"
    [[ -f "$BACKUP_DIR/pubkey-secp-bls" ]] && cp -a "$BACKUP_DIR/pubkey-secp-bls" "$MONAD_HOME/"
    fix_ownership
    ok "Keys and config restored from backup"
    save_state "last_step" "2"
  fi

  # ── 3. Restore keystore password ──
  if ! $RESUME || ! completed_step 3; then
    step "RESTORE KEYSTORE PASSWORD"
    if [[ -f "$BACKUP_DIR/keystore-password-backup" ]]; then
      local backup_pw
      backup_pw="$(cat "$BACKUP_DIR/keystore-password-backup")"
      # shellcheck disable=SC1090
      source "$ENV_FILE"
      if [[ "${KEYSTORE_PASSWORD:-}" != "$backup_pw" ]]; then
        if grep -q '^KEYSTORE_PASSWORD=' "$ENV_FILE" 2>/dev/null; then
          awk -v pw="$backup_pw" '/^KEYSTORE_PASSWORD=/{print "KEYSTORE_PASSWORD='"'"'" pw "'"'"'"; next} {print}' \
            "$ENV_FILE" > "${ENV_FILE}.tmp" && mv "${ENV_FILE}.tmp" "$ENV_FILE"
        else
          printf "KEYSTORE_PASSWORD='%s'\n" "$backup_pw" >> "$ENV_FILE"
        fi
        chmod 600 "$ENV_FILE"
        KEYSTORE_PASSWORD="$backup_pw"
        ok "KEYSTORE_PASSWORD restored from backup"
      else
        ok "KEYSTORE_PASSWORD already matches"
      fi
    else
      warn "No keystore-password-backup found in backup"
      echo "  If .env password doesn't match the restored keys, update it manually."
      # shellcheck disable=SC1090
      source "$ENV_FILE"
    fi
    [[ -n "${KEYSTORE_PASSWORD:-}" ]] || die "KEYSTORE_PASSWORD not set"
    save_state "last_step" "3"
  fi

  # ── 4. Verify restored identity ──
  if ! $RESUME || ! completed_step 4; then
    step "VERIFY RESTORED IDENTITY"

    SECP_PUB="$(monad-keystore recover \
      --password "$KEYSTORE_PASSWORD" \
      --keystore-path "$SECP_KEY" \
      --key-type secp 2>/dev/null | grep -i 'Secp public key' | awk '{print $NF}' || true)"
    BLS_PUB="$(monad-keystore recover \
      --password "$KEYSTORE_PASSWORD" \
      --keystore-path "$BLS_KEY" \
      --key-type bls 2>/dev/null | grep -i 'BLS public key' | awk '{print $NF}' || true)"

    [[ -n "$SECP_PUB" ]] || die "Could not recover SECP key — password mismatch?"
    [[ -n "$BLS_PUB" ]]  || die "Could not recover BLS key — password mismatch?"

    echo "  Restored SECP: ${SECP_PUB:0:46}..."
    echo "  Restored BLS:  ${BLS_PUB:0:46}..."
    echo
    confirm_yn "Is this the box's original full-node identity?" \
      || die "Wrong identity — check backup selection."
    ok "Identity confirmed"

    detect_network "$NODE_TOML"

    save_state "secp_pub" "$SECP_PUB"
    save_state "bls_pub" "$BLS_PUB"
    save_state "network" "$NETWORK"
    save_state "last_step" "4"
  fi

  # ── 5. Sign name record + patch ──
  if ! $RESUME || ! completed_step 5; then
    local cur_seq backup_seq eff_seq
    cur_seq="$(grep '^self_record_seq_num' "$NODE_TOML" 2>/dev/null \
      | awk '{print $NF}' | tr -d '"' || true)"
    cur_seq="${cur_seq:-0}"
    backup_seq="$(grep '^self_record_seq_num' "$BACKUP_DIR/node.toml" 2>/dev/null \
      | awk '{print $NF}' | tr -d '"' || true)"
    backup_seq="${backup_seq:-0}"
    # Use whichever is higher — identity may have migrated after backup was taken
    eff_seq=$(( cur_seq > backup_seq ? cur_seq : backup_seq ))
    local new_seq=$((eff_seq + 1))
    ok "Name record seq: max(current=$cur_seq, backup=$backup_seq) + 1 = $new_seq"

    detect_ip
    sign_and_patch "$NODE_TOML" "$IP" "$new_seq" "$SECP_KEY" "$KEYSTORE_PASSWORD" "false"
    save_state "last_step" "5"
  fi

  # ── 6. Start as full node ──
  if ! $RESUME || ! completed_step 6; then
    start_monad_services
    save_state "last_step" "6"
  fi

  # ── 7. Verify ──
  if ! $RESUME || ! completed_step 7; then
    post_verify
    save_state "last_step" "7"
  fi

  clear_state
  echo
  bar
  ok "${BOLD}FULL-NODE IDENTITY RESTORED${RESET}"
  bar

  echo
  echo "  This box is running as a full node with its original identity."
  echo "  The validator keys have been overwritten with backed-up full-node keys."
  echo
  echo "${BOLD}MONITOR${RESET}"
  echo "  monad-status"
  echo "  journalctl -fu monad-bft"
  echo
}

# ══════════════════════════════════════════════════════════
# CLI
# ══════════════════════════════════════════════════════════
usage() {
  echo "monad-failover v${VERSION} — Monad validator mobility toolkit"
  echo
  echo "Usage:"
  echo "  ./monad-failover.sh promote          [--peer-host user@host] [--resume]"
  echo "  ./monad-failover.sh prepare-standby  [--snapshot-reset] [--resume]"
  echo "  ./monad-failover.sh restore-fullnode [--resume]"
  echo
  echo "Modes:"
  echo "  promote          Promote a synced full node to validator"
  echo "  prepare-standby  Sync a box as a full node under temporary keys"
  echo "  restore-fullnode Restore a box to its original full-node identity"
  echo
  echo "Flags:"
  echo "  --peer-host      SSH target to verify the old validator is stopped (promote only)"
  echo "  --snapshot-reset Hard reset from snapshot (prepare-standby only)"
  echo "  --resume         Continue from last completed step"
  exit 0
}

MODE=""
RESUME=false
PEER_HOST=""
SNAPSHOT_RESET=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    promote|prepare-standby|restore-fullnode) MODE="$1" ;;
    --resume)         RESUME=true ;;
    --peer-host)      shift; PEER_HOST="${1:-}"; [[ -n "$PEER_HOST" ]] || die "--peer-host requires a value" ;;
    --snapshot-reset) SNAPSHOT_RESET=true ;;
    -h|--help|help)   usage ;;
    *)                die "Unknown argument: $1. Run with --help for usage." ;;
  esac
  shift
done

[[ -n "$MODE" ]] || usage

# ── main ───────────────────────────────────────────────────
init_state

# Migrate v1 state file if present
if [[ -f "$MONAD_HOME/.monad-failover/state" ]] && [[ ! -d "$MONAD_HOME/.monad-failover/promote" ]]; then
  warn "Found v1 state file — removing (use v2 --resume per mode)"
  rm -f "$MONAD_HOME/.monad-failover/state"
fi

# Check for incomplete previous run
if ! $RESUME && [[ -f "$STATE_FILE" ]]; then
  _last="$(load_state "last_step")"
  if [[ -n "$_last" ]]; then
    warn "Previous $MODE run stopped at step $_last"
    echo "  Run with --resume to continue, or start fresh."
    confirm_yn "  Start fresh?" && clear_state || { echo "  Use: ./monad-failover.sh $MODE --resume"; exit 0; }
  fi
fi

clear 2>/dev/null || true

case "$MODE" in
  promote)          mode_promote ;;
  prepare-standby)  mode_prepare_standby ;;
  restore-fullnode) mode_restore_fullnode ;;
esac
