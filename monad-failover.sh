#!/usr/bin/env bash
#
# monad-failover — promote a synced Monad full node to validator
#
# Follows the official Node Migration procedure:
#   https://docs.monad.xyz/node-ops/node-recovery/node-migration
#
# Usage:
#   ./monad-failover.sh [--backup-dir PATH] [--peer-host user@host] [--resume]

set -euo pipefail

VERSION="4.0.0"

# ── paths ──────────────────────────────────────────────────
MONAD_HOME="/home/monad"
CONFIG_DIR="$MONAD_HOME/monad-bft/config"
NODE_TOML="$CONFIG_DIR/node.toml"
ENV_FILE="$MONAD_HOME/.env"
SECP_KEY="$CONFIG_DIR/id-secp"
BLS_KEY="$CONFIG_DIR/id-bls"
SECP_KEY_NEW="$CONFIG_DIR/id-secp.new"
BLS_KEY_NEW="$CONFIG_DIR/id-bls.new"
BACKUP_ROOT="/opt/monad/backup"
MONAD_SERVICES=(monad-bft monad-execution monad-rpc)

# ── ui helpers ─────────────────────────────────────────────
BOLD=$'\033[1m' DIM=$'\033[2m'
RED=$'\033[31m' GREEN=$'\033[32m' YELLOW=$'\033[33m' CYAN=$'\033[36m'
RESET=$'\033[0m'

header() {
  echo
  echo "╔══════════════════════════════════════════════╗"
  echo "║        MONAD VALIDATOR FAILOVER TOOL         ║"
  echo "╚══════════════════════════════════════════════╝"
  echo
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
STATE_DIR="$MONAD_HOME/.monad-failover"
STATE_FILE="$STATE_DIR/state"

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

# ── guards ─────────────────────────────────────────────────
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

# Official Monad RPC ports (8080/8081) plus commonly exposed EVM RPC ports.
RPC_PORTS=(8080 8081 8545 8546 9545 9546 18545 18546)

check_rpc() {
  step "RPC EXPOSURE CHECK"
  local listeners exposed=()
  listeners="$(ss -ltn 2>/dev/null | awk '{print $4}' || true)"
  for port in "${RPC_PORTS[@]}"; do
    if echo "$listeners" | grep -qE "^(0\.0\.0\.0|\*|\[::\]):${port}$"; then
      exposed+=("$port")
    fi
  done
  if [[ ${#exposed[@]} -gt 0 ]]; then
    warn "Publicly listening RPC ports: ${exposed[*]}"
    echo "  Validators should not expose RPC publicly."
    echo "  Bind to localhost or block these ports with a firewall."
  else
    ok "No RPC ports publicly exposed (checked: ${RPC_PORTS[*]})"
  fi
}

detect_network() {
  NETWORK="$(grep '^network_name' "$NODE_TOML" 2>/dev/null | cut -d '"' -f2 || true)"
  if [[ -z "$NETWORK" ]]; then
    read -r -p "Could not detect network. Enter (mainnet/testnet): " NETWORK
    [[ "$NETWORK" == "mainnet" || "$NETWORK" == "testnet" ]] || die "Invalid network"
  fi
  ok "Network: $NETWORK"
}

run_location_guard() {
  bar
  warn "This will ${BOLD}promote this full node to validator${RESET}."
  echo "  Hostname: ${BOLD}$(hostname)${RESET}"

  local ip
  ip="$(curl -s4 --connect-timeout 5 ifconfig.me || true)"
  [[ -n "$ip" ]] && echo "  Public IP: ${BOLD}${ip}${RESET}"

  if [[ -f "$SECP_KEY" ]] && [[ -n "${KEYSTORE_PASSWORD:-}" ]]; then
    local cur_pub
    cur_pub="$(recover_pubkey "$SECP_KEY" secp)"
    [[ -n "$cur_pub" ]] && echo "  Current SECP: ${cur_pub:0:24}..."
  fi

  echo
  confirm_yn "Is this the correct target host?" || die "Aborted."
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

# ── key helpers ────────────────────────────────────────────
recover_pubkey() {
  local keypath="$1" keytype="$2" label
  [[ "$keytype" == "secp" ]] && label='Secp public key' || label='BLS public key'
  monad-keystore recover \
    --password "$KEYSTORE_PASSWORD" \
    --keystore-path "$keypath" \
    --key-type "$keytype" 2>/dev/null \
    | grep -i "$label" | awk '{print $NF}' || true
}

validate_ikm() {
  local ikm="${1#0x}"
  [[ "$ikm" =~ ^[0-9a-fA-F]{64}$ ]] || return 1
  printf '%s' "$ikm"
}

# Extract the IKM secret from an official key backup file
# (format produced by `monad-keystore recover`, per docs.monad.xyz).
extract_ikm_from_backup() {
  grep -E "Keystore secret:|Keep your IKM secure:" "$1" 2>/dev/null \
    | awk '{print $NF}' | head -1 || true
}

import_staged_key() {
  local ikm="$1" keypath="$2" keytype="$3"
  monad-keystore import \
    --ikm "$ikm" \
    --keystore-path "$keypath" \
    --password "$KEYSTORE_PASSWORD"
  chown monad:monad "$keypath" 2>/dev/null || true
  chmod 600 "$keypath" 2>/dev/null || true
}

# ── config helpers ─────────────────────────────────────────
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

sign_and_patch() {
  local toml="$1" ip="$2" seq="$3" keypath="$4"

  sanitize_placeholders "$toml"

  step "SIGN NAME RECORD (seq $seq)"
  local sign_out
  if ! sign_out="$(monad-sign-name-record \
    --address "$ip:8000" \
    --node-config "$toml" \
    --authenticated-udp-port 8001 \
    --self-record-seq-num "$seq" \
    --keystore-path "$keypath" \
    --password "$KEYSTORE_PASSWORD")"; then
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

  grep -q "^self_address = \"$SELF_ADDRESS\"" "$toml" || die "Failed to write self_address"
  grep -q "^self_record_seq_num = $seq" "$toml"        || die "Failed to write self_record_seq_num"
  fix_ownership
  ok "node.toml patched and verified"
}

backup_config() {
  step "BACKUP CURRENT CONFIG"
  local ts
  ts="$(date +%Y%m%d-%H%M%S)"
  BACKUP_DIR="$BACKUP_ROOT/failover-${ts}"
  mkdir -p "$BACKUP_DIR"

  for f in node.toml id-secp id-bls; do
    [[ -f "$CONFIG_DIR/$f" ]] && cp -a "$CONFIG_DIR/$f" "$BACKUP_DIR/"
  done
  [[ -f "$MONAD_HOME/pubkey-secp-bls" ]] && cp -a "$MONAD_HOME/pubkey-secp-bls" "$BACKUP_DIR/"
  [[ -f "$ENV_FILE" ]] && cp -a "$ENV_FILE" "$BACKUP_DIR/"

  if [[ -n "${KEYSTORE_PASSWORD:-}" ]]; then
    printf '%s' "$KEYSTORE_PASSWORD" > "$BACKUP_DIR/keystore-password-backup"
    chmod 600 "$BACKUP_DIR/keystore-password-backup"
  fi

  ok "Config backed up to $BACKUP_DIR"
  save_state "backup_dir" "$BACKUP_DIR"
}

# Re-export official-format key backups from the live validator keys
# (same format and paths as the full node installation guide).
refresh_key_backups() {
  step "REFRESH KEY BACKUPS"
  mkdir -p "$BACKUP_ROOT"
  local ts
  ts="$(date +%Y%m%d%H%M%S)"

  [[ -f "$BACKUP_ROOT/secp-backup" ]] && mv "$BACKUP_ROOT/secp-backup" "$BACKUP_ROOT/secp-backup.${ts}.bak"
  [[ -f "$BACKUP_ROOT/bls-backup" ]]  && mv "$BACKUP_ROOT/bls-backup"  "$BACKUP_ROOT/bls-backup.${ts}.bak"

  monad-keystore recover \
    --password "$KEYSTORE_PASSWORD" \
    --keystore-path "$SECP_KEY" \
    --key-type secp > "$BACKUP_ROOT/secp-backup"
  monad-keystore recover \
    --password "$KEYSTORE_PASSWORD" \
    --keystore-path "$BLS_KEY" \
    --key-type bls > "$BACKUP_ROOT/bls-backup"
  chmod 600 "$BACKUP_ROOT/secp-backup" "$BACKUP_ROOT/bls-backup"

  ok "Key backups exported: $BACKUP_ROOT/{secp-backup,bls-backup}"
  warn "Store copies of both files OUTSIDE this server (password manager / vault)."
  echo "  They are the only way to recover this validator's identity."
}

post_verify() {
  step "POST-CUTOVER VERIFICATION"
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

# ══════════════════════════════════════════════════════════
# PROMOTE — full node → validator
# ══════════════════════════════════════════════════════════
promote() {
  header

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
      warn "No previous run found. Starting fresh."; RESUME=false
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
    run_location_guard
    save_state "network" "$NETWORK"
    save_state "last_step" "2"
  fi

  # ── 3. Backup this server's own identity ──
  if ! $RESUME || ! completed_step 3; then
    backup_config
    save_state "last_step" "3"
  fi

  # ── 4. Import validator keys (staging — live keys untouched) ──
  if ! $RESUME || ! completed_step 4; then
    rm -f "$SECP_KEY_NEW" "$BLS_KEY_NEW"

    bar
    echo "${BOLD}VALIDATOR KEY IMPORT${RESET}"
    echo "  Keys are imported to staging files (id-secp.new / id-bls.new)."
    echo "  Live keys remain untouched until cutover."
    echo

    local SECP_IKM="" BLS_IKM=""

    if [[ -z "$KEY_SOURCE_DIR" ]]; then
      echo "  How do you want to provide the validator keys?"
      echo
      echo "    1) Key backup files (secp-backup / bls-backup) — ${GREEN}recommended${RESET}"
      echo "       Works even when the old server is unreachable."
      echo "    2) Paste IKM hex values manually (hidden input)"
      echo
      read -r -p "  Select (1/2): " key_choice
      case "$key_choice" in
        1) read -r -p "  Backup directory [$BACKUP_ROOT]: " KEY_SOURCE_DIR
           KEY_SOURCE_DIR="${KEY_SOURCE_DIR:-$BACKUP_ROOT}" ;;
        2) KEY_SOURCE_DIR="-" ;;
        *) die "Invalid selection" ;;
      esac
    fi

    if [[ "$KEY_SOURCE_DIR" != "-" ]]; then
      step "READ KEY BACKUP FILES"
      local secp_file="$KEY_SOURCE_DIR/secp-backup"
      local bls_file="$KEY_SOURCE_DIR/bls-backup"
      [[ -f "$secp_file" ]] || die "Not found: $secp_file"
      [[ -f "$bls_file" ]]  || die "Not found: $bls_file"

      SECP_IKM="$(extract_ikm_from_backup "$secp_file")"
      BLS_IKM="$(extract_ikm_from_backup "$bls_file")"

      SECP_IKM="$(validate_ikm "$SECP_IKM")" \
        || die "Could not extract a valid SECP IKM from $secp_file"
      BLS_IKM="$(validate_ikm "$BLS_IKM")" \
        || die "Could not extract a valid BLS IKM from $bls_file"
      ok "IKM secrets extracted from backup files"
    else
      echo "Paste the validator IKM hex values. Input is hidden."
      echo
      read -r -s -p "SECP IKM_HEX: " SECP_IKM; echo
      SECP_IKM="$(validate_ikm "$SECP_IKM")" || die "SECP IKM must be 64 hex characters"
      read -r -s -p "BLS  IKM_HEX: " BLS_IKM; echo
      BLS_IKM="$(validate_ikm "$BLS_IKM")" || die "BLS IKM must be 64 hex characters"
    fi

    step "Importing SECP key (staging)"
    import_staged_key "$SECP_IKM" "$SECP_KEY_NEW" secp
    ok "SECP key imported to id-secp.new"

    step "Importing BLS key (staging)"
    import_staged_key "$BLS_IKM" "$BLS_KEY_NEW" bls
    ok "BLS key imported to id-bls.new"
    SECP_IKM="" BLS_IKM=""

    bar
    echo "${BOLD}VERIFY KEYS${RESET}"

    SECP_PUB="$(recover_pubkey "$SECP_KEY_NEW" secp)"
    BLS_PUB="$(recover_pubkey "$BLS_KEY_NEW" bls)"

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

  # ── 5. Beneficiary + seq + config flags ──
  if ! $RESUME || ! completed_step 5; then
    bar
    echo "${BOLD}BENEFICIARY${RESET}"
    echo "Enter the beneficiary address from the old validator's node.toml"
    read -r -p "beneficiary: " BENEFICIARY
    if [[ -n "$BENEFICIARY" ]]; then
      set_toml_value "$NODE_TOML" "beneficiary" "\"$BENEFICIARY\""
      ok "Beneficiary: $BENEFICIARY"
    else
      warn "No beneficiary entered — check node.toml manually after promotion."
    fi

    bar
    echo "${BOLD}NODE NAME${RESET}"
    echo "Per the migration docs, this node should take over the old validator's"
    echo "node_name during migration. Leave empty to keep the current name."
    read -r -p "node_name: " NODE_NAME
    if [[ -n "$NODE_NAME" ]]; then
      set_toml_value "$NODE_TOML" "node_name" "\"$NODE_NAME\""
      ok "node_name: $NODE_NAME"
    else
      ok "node_name unchanged"
    fi

    bar
    echo "${BOLD}SEQ NUM${RESET}"
    echo "Enter the last self_record_seq_num used by this validator identity"
    echo "(from the old validator's node.toml, or your records; 0 if never migrated)."
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
    sign_and_patch "$NODE_TOML" "$IP" "$NEW_SEQ" "$SECP_KEY_NEW"

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
      warn "The old validator must be ${BOLD}STOPPED or DOWN${RESET} before proceeding."
      echo "  If the server is reachable, run on it:"
      echo "    systemctl stop monad-bft monad-execution monad-rpc"
      echo "  If the server is dead/unreachable, ensure it cannot come back"
      echo "  online with the old keys (power it off at the provider if needed)."
      echo
      confirm_yn "Old validator confirmed stopped or down?" || die "Aborted."
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

  # ── 8. Verify + refresh key backups ──
  if ! $RESUME || ! completed_step 8; then
    post_verify
    refresh_key_backups
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
  warn "VDP: validators are required to push metrics to Monad Foundation's"
  echo "  monitoring infrastructure. Make sure this server is pushing them:"
  echo "  https://docs.monad.xyz/node-ops/validator-delegation-program"
  echo
}

# ══════════════════════════════════════════════════════════
# CLI
# ══════════════════════════════════════════════════════════
usage() {
  echo "monad-failover v${VERSION} — promote a synced Monad full node to validator"
  echo
  echo "Usage:"
  echo "  ./monad-failover.sh [--backup-dir PATH] [--peer-host user@host] [--resume]"
  echo
  echo "Flags:"
  echo "  --backup-dir  Directory containing secp-backup / bls-backup key files"
  echo "                (skips the interactive key-source prompt)"
  echo "  --peer-host   SSH target to verify the old validator is stopped"
  echo "  --resume      Continue from the last completed step"
  exit 0
}

RESUME=false
PEER_HOST=""
KEY_SOURCE_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --resume)     RESUME=true ;;
    --peer-host)  shift; PEER_HOST="${1:-}"; [[ -n "$PEER_HOST" ]] || die "--peer-host requires a value" ;;
    --backup-dir) shift; KEY_SOURCE_DIR="${1:-}"; [[ -n "$KEY_SOURCE_DIR" ]] || die "--backup-dir requires a value" ;;
    -h|--help|help) usage ;;
    *)            die "Unknown argument: $1. Run with --help for usage." ;;
  esac
  shift
done

# ── main ───────────────────────────────────────────────────
mkdir -p "$STATE_DIR"

# Adopt v3 promote state (per-mode dirs) so --resume keeps working across the upgrade
if [[ ! -f "$STATE_FILE" ]] && [[ -f "$STATE_DIR/promote/state" ]]; then
  mv "$STATE_DIR/promote/state" "$STATE_FILE"
fi
rm -rf "$STATE_DIR/promote" "$STATE_DIR/prepare-standby" "$STATE_DIR/restore-fullnode" 2>/dev/null || true

if ! $RESUME && [[ -f "$STATE_FILE" ]]; then
  _last="$(load_state "last_step")"
  if [[ -n "$_last" ]]; then
    warn "Previous run stopped at step $_last"
    echo "  Run with --resume to continue, or start fresh."
    confirm_yn "  Start fresh?" && clear_state || { echo "  Use: ./monad-failover.sh --resume"; exit 0; }
  fi
fi

clear 2>/dev/null || true
promote
