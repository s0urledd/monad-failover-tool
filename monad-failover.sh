#!/usr/bin/env bash
#
# monad-failover — promote a synced Monad full node to validator
#
# Follows the official Node Migration procedure:
#   https://docs.monad.xyz/node-ops/node-recovery/node-migration
#
# Usage:
#   ./monad-failover.sh [--backup-dir PATH] [--resume]
#   ./monad-failover.sh --dry-run   # read-only preflight, changes nothing

set -euo pipefail

# Secrets (key backups, state) must never be created world-readable, not even
# for the instant between open() and chmod. Restrict from the start.
umask 077

VERSION="1.2.0"

# ── paths (env-overridable for testing) ────────────────────
MONAD_HOME="${MONAD_HOME:-/home/monad}"
CONFIG_DIR="$MONAD_HOME/monad-bft/config"
NODE_TOML="$CONFIG_DIR/node.toml"
ENV_FILE="$MONAD_HOME/.env"
SECP_KEY="$CONFIG_DIR/id-secp"
BLS_KEY="$CONFIG_DIR/id-bls"
SECP_KEY_NEW="$CONFIG_DIR/id-secp.new"
BLS_KEY_NEW="$CONFIG_DIR/id-bls.new"
BACKUP_ROOT="${BACKUP_ROOT:-/opt/monad/backup}"
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
die() {
  printf '%b✗%b %s\n' "$RED" "$RESET" "${1:-Aborted.}" >&2
  shift || true
  local line
  for line in "$@"; do printf '   %s\n' "$line" >&2; done
  exit 1
}
need_cmd() { command -v "$1" >/dev/null 2>&1 || die "Missing command: $1"; }

confirm_yn() {
  local prompt="$1"
  read -r -p "$prompt (y/N): " ans
  case "${ans,,}" in y|yes) return 0 ;; *) return 1 ;; esac
}

# Read KEYSTORE_PASSWORD from .env WITHOUT sourcing it. .env is owned by the
# monad user; sourcing it as root would execute anything a compromised monad
# account placed there. We only ever need this one value.
load_keystore_password() {
  local line val
  line="$(grep -m1 '^KEYSTORE_PASSWORD=' "$ENV_FILE" 2>/dev/null || true)"
  [[ -n "$line" ]] || return 1
  val="${line#KEYSTORE_PASSWORD=}"
  if [[ "$val" == \'*\' ]]; then
    val="${val#\'}"; val="${val%\'}"
  elif [[ "$val" == \"*\" ]]; then
    val="${val#\"}"; val="${val%\"}"
  fi
  KEYSTORE_PASSWORD="$val"
}

# Detect the public IPv4 over HTTPS and validate it. Echoes the IP or nothing.
public_ip() {
  local ip
  ip="$(curl -fsS4 --connect-timeout 10 https://ifconfig.me 2>/dev/null || true)"
  if [[ "$ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
    printf '%s' "$ip"
  fi
}

# Escape text for safe use as the replacement side of a sed s||| command
# (delimiter |, plus & and backslash). Prevents node.toml corruption or
# command execution from crafted beneficiary/node_name/address values.
sed_escape_replacement() {
  printf '%s' "$1" | sed -e 's/[&\\|]/\\&/g'
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
  [[ "$c" =~ ^[0-9]+$ ]] && [[ "$c" -ge "$1" ]]
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
  NETWORK="$(grep -m1 '^network_name' "$NODE_TOML" 2>/dev/null | cut -d '"' -f2 || true)"
  if [[ "$NETWORK" != "mainnet" && "$NETWORK" != "testnet" ]]; then
    read -r -p "Network could not be detected. Enter (mainnet/testnet): " NETWORK
    [[ "$NETWORK" == "mainnet" || "$NETWORK" == "testnet" ]] || die "Invalid network"
  fi
  ok "Network: $NETWORK"
}

run_location_guard() {
  bar
  warn "This will ${BOLD}promote this full node to validator${RESET}."
  echo "  Hostname: ${BOLD}$(hostname)${RESET}"

  local ip
  ip="$(public_ip)"
  [[ -n "$ip" ]] && echo "  Public IP: ${BOLD}${ip}${RESET}"

  echo
  confirm_yn "Is this the correct target host?" || die "Aborted."
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
    | awk '{print $NF}' | tr -d '\r' | head -1 || true
}

import_staged_key() {
  local ikm="$1" keypath="$2"
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
  local esc; esc="$(sed_escape_replacement "$value")"

  if grep -qE "^[[:space:]]*${key}[[:space:]]*=" "$file" 2>/dev/null; then
    sed -i "s|^[[:space:]]*${key}[[:space:]]*=.*|${key} = ${esc}|" "$file"
  elif [[ -z "$section" ]]; then
    die "Key '$key' not found in $file and no section given — refusing to append blindly."
  elif grep -qF "[$section]" "$file"; then
    sed -i "\\|^\\[$section\\]|a ${key} = ${esc}" "$file"
  else
    die "Section [$section] not found in $file — cannot place '$key'."
  fi

  grep -qF "${key} = ${value}" "$file" || die "Failed to write '$key' to $file"
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
  local esc_addr esc_sig
  esc_addr="$(sed_escape_replacement "$SELF_ADDRESS")"
  esc_sig="$(sed_escape_replacement "$SELF_SIG")"
  sed -i "s|^self_address.*|self_address = \"$esc_addr\"|" "$toml"
  sed -i "s|^self_record_seq_num.*|self_record_seq_num = $seq|" "$toml"
  sed -i "s|^self_name_record_sig.*|self_name_record_sig = \"$esc_sig\"|" "$toml"

  grep -qF "self_address = \"$SELF_ADDRESS\"" "$toml" || die "Failed to write self_address"
  grep -qF "self_record_seq_num = $seq" "$toml"        || die "Failed to write self_record_seq_num"
  fix_ownership
  ok "node.toml patched and verified"
}

backup_config() {
  step "BACKUP CURRENT CONFIG"
  local ts
  ts="$(date +%Y%m%d-%H%M%S)"
  BACKUP_DIR="$BACKUP_ROOT/failover-${ts}"
  mkdir -p "$BACKUP_DIR"
  chmod 700 "$BACKUP_DIR" 2>/dev/null || true

  # Preserve this server's own identity (encrypted keystores + config) so it
  # can be restored to a full node by hand. The keystore password is NOT copied
  # here — colocating it with the encrypted keys would defeat the encryption.
  for f in node.toml id-secp id-bls; do
    [[ -f "$CONFIG_DIR/$f" ]] && cp -a "$CONFIG_DIR/$f" "$BACKUP_DIR/"
  done
  [[ -f "$MONAD_HOME/pubkey-secp-bls" ]] && cp -a "$MONAD_HOME/pubkey-secp-bls" "$BACKUP_DIR/"

  ok "Config backed up to $BACKUP_DIR"
  save_state "backup_dir" "$BACKUP_DIR"
}

# Export one key to an official-format backup file, atomically (temp + rename)
# so a failure never leaves a truncated file in place. Returns nonzero on error.
export_key_backup() {
  local keypath="$1" keytype="$2" out="$3"
  if monad-keystore recover \
      --password "$KEYSTORE_PASSWORD" \
      --keystore-path "$keypath" \
      --key-type "$keytype" > "${out}.partial" 2>/dev/null; then
    mv "${out}.partial" "$out"
    chmod 600 "$out"
    return 0
  fi
  rm -f "${out}.partial"
  return 1
}

# Re-export official-format key backups from the live validator keys
# (same format and paths as the full node installation guide).
refresh_key_backups() {
  step "REFRESH KEY BACKUPS"
  mkdir -p "$BACKUP_ROOT"
  chmod 700 "$BACKUP_ROOT" 2>/dev/null || true
  local ts
  ts="$(date +%Y%m%d%H%M%S)"
  local se="$BACKUP_ROOT/secp-backup" bl="$BACKUP_ROOT/bls-backup"

  [[ -f "$se" ]] && mv "$se" "$se.${ts}.bak"
  [[ -f "$bl" ]] && mv "$bl" "$bl.${ts}.bak"

  if export_key_backup "$SECP_KEY" secp "$se" && export_key_backup "$BLS_KEY" bls "$bl"; then
    ok "Key backups exported: $BACKUP_ROOT/{secp-backup,bls-backup}"
    warn "Store copies of both files OUTSIDE this server (password manager / vault)."
    echo "  They are the only way to recover this validator's identity."
  else
    warn "Could not re-export key backups."
    echo "  Previous copies are preserved as *.${ts}.bak in $BACKUP_ROOT."
    echo "  Re-run later with: ./monad-failover.sh --resume"
  fi
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

# ── validator uptime API (monval by Huginn) ────────────────
VALIDATOR_API_MAINNET="https://validator-api.huginn.tech/monad-api/validator/uptime"
VALIDATOR_API_TESTNET="https://validator-api-testnet.huginn.tech/monad-api/validator/uptime"

validator_api_url() {
  local base
  [[ "$NETWORK" == "mainnet" ]] && base="$VALIDATOR_API_MAINNET" || base="$VALIDATOR_API_TESTNET"
  printf '%s/%s' "$base" "$SECP_PUB"
}

json_str() { printf '%s' "$1" | grep -o "\"$2\": *\"[^\"]*\"" | head -1 | sed 's/.*: *"//; s/"$//'; }
json_num() { printf '%s' "$1" | grep -o "\"$2\": *[0-9.]*" | head -1 | sed 's/.*: *//'; }

# Ask the network how it sees this validator. Never blocks the flow: on any
# failure it prints the URL to check later and returns 0.
check_validator_api() {
  step "VALIDATOR UPTIME CHECK"
  local url resp
  url="$(validator_api_url)"
  resp="$(curl -fsS --connect-timeout 10 "$url" 2>/dev/null || true)"

  if ! printf '%s' "$resp" | grep -qE '"success": *true'; then
    warn "Validator not visible in the uptime API yet (this can take a few minutes)."
    echo "  Check later: $url"
    return 0
  fi

  local name status uptime fin to last_round
  name="$(json_str "$resp" validator_name)"
  status="$(json_str "$resp" status)"
  uptime="$(json_num "$resp" uptime_percent)"
  fin="$(json_num "$resp" finalized_count)"
  to="$(json_num "$resp" timeout_count)"
  last_round="$(json_num "$resp" last_round)"

  ok "${name:-validator} is ${BOLD}${status:-unknown}${RESET} on $NETWORK"
  echo "  Uptime (24h): ${uptime:-?}% (${fin:-?} finalized, ${to:-?} timeout)"
  [[ -n "$last_round" ]] && echo "  Last round:   $last_round"
}

detect_ip() {
  IP="$(public_ip)"
  [[ -n "$IP" ]] || die "Could not detect a valid public IPv4 address."
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

  load_keystore_password || die "KEYSTORE_PASSWORD not set in $ENV_FILE"
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
    import_staged_key "$SECP_IKM" "$SECP_KEY_NEW"
    ok "SECP key imported to id-secp.new"

    step "Importing BLS key (staging)"
    import_staged_key "$BLS_IKM" "$BLS_KEY_NEW"
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
      [[ "$BENEFICIARY" =~ ^0x[0-9a-fA-F]{40}$ ]] \
        || die "beneficiary must be a 0x-prefixed 40-hex-character address"
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
      [[ "$NODE_NAME" =~ ^[A-Za-z0-9._-]{1,64}$ ]] \
        || die "node_name may contain only letters, digits, dot, dash, underscore (max 64)"
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

    bar
    warn "The old validator MUST be ${BOLD}stopped or fully offline${RESET} before cutover."
    echo "  Running two nodes with the same keys corrupts this validator's"
    echo "  consensus participation and name record. This is the one step you"
    echo "  cannot take back — get it right."
    echo
    echo "  • If the old server is reachable, stop it now:"
    echo "      ${BOLD}systemctl stop monad-bft monad-execution monad-rpc${RESET}"
    echo "    then confirm it is down:"
    echo "      systemctl is-active monad-bft monad-execution monad-rpc   # expect: inactive"
    echo "  • If the old server is dead or unreachable, make sure it cannot come"
    echo "    back online with these keys (power it off at your provider)."
    echo
    echo "  Type ${BOLD}STOPPED${RESET} (in capitals) to confirm the old validator is down."
    read -r -p "  > " confirm_stopped
    [[ "$confirm_stopped" == "STOPPED" ]] || die "Not confirmed — aborting before cutover."
    ok "Old validator confirmed stopped or offline"

    bar
    warn "${BOLD}POINT OF NO RETURN${RESET}"
    echo "  The next step stops services, swaps in the validator keys, and starts."
    echo "  After this the old validator MUST NOT be restarted with the same keys."
    echo
    confirm_yn "Proceed with cutover?" || die "Aborted."

    step "CUTOVER"
    # Both staging keys must exist before we touch services — never leave a
    # half-swapped identity.
    [[ -f "$SECP_KEY_NEW" && -f "$BLS_KEY_NEW" ]] || die \
      "Staging keys are missing — refusing to stop services." \
      "Start a fresh run so the key-import step re-creates them."

    stop_monad_services

    mv -f "$SECP_KEY_NEW" "$SECP_KEY" || die \
      "Could not place the SECP key. Services are stopped; keys are unchanged." \
      "Fix the cause, then start a fresh run to re-stage the keys."
    if ! mv -f "$BLS_KEY_NEW" "$BLS_KEY"; then
      die "CRITICAL: the SECP key was placed but the BLS key was not." \
        "This node now has a mismatched identity. Do NOT start the services." \
        "Start a fresh run to re-stage both keys, or restore this node's" \
        "previous identity from: ${BACKUP_DIR:-$BACKUP_ROOT}"
    fi
    fix_ownership
    systemctl enable "${MONAD_SERVICES[@]}" 2>/dev/null || true

    # The key swap is done. If the services fail to start, record step 7 as
    # complete FIRST so --resume never repeats the (now impossible) swap, then
    # hand the operator the exact recovery commands.
    if ! systemctl start "${MONAD_SERVICES[@]}"; then
      save_state "last_step" "7"
      die "The validator keys are in place, but the services failed to start." \
        "The swap is done — do NOT re-run the cutover." \
        "Diagnose: journalctl -xeu monad-bft" \
        "Start when fixed: systemctl start ${MONAD_SERVICES[*]}" \
        "Then finish up:  ./monad-failover.sh --resume"
    fi
    ok "Services started"

    save_state "last_step" "7"
  fi

  # ── 8. Verify + refresh key backups ──
  if ! $RESUME || ! completed_step 8; then
    post_verify
    check_validator_api
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
  echo "  Live (proposed / finalized / timeout events once in the active set):"
  echo "    journalctl -u monad-ledger-tail -o cat -f | grep -i \"${SECP_PUB}\""
  echo "  Uptime API:"
  echo "    curl $(validator_api_url)"

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
# DRY RUN — read-only preflight, changes nothing
# ══════════════════════════════════════════════════════════
mode_dry_run() {
  header
  echo "${BOLD}DRY RUN${RESET} — read-only preflight. No files, keys or services are touched."
  local fails=0 warns=0

  step "REQUIRED COMMANDS"
  local c
  for c in curl systemctl sed monad-keystore monad-sign-name-record; do
    if command -v "$c" >/dev/null 2>&1; then
      ok "$c"
    else
      echo "${RED}✗${RESET} missing: $c"; fails=$((fails + 1))
    fi
  done
  if command -v monad-status >/dev/null 2>&1; then
    ok "monad-status"
  else
    warn "monad-status not installed — sync gate will need manual confirmation"
    warns=$((warns + 1))
  fi

  step "FILES & ENVIRONMENT"
  if [[ -f "$NODE_TOML" ]]; then ok "node.toml"; else
    echo "${RED}✗${RESET} missing: $NODE_TOML"; fails=$((fails + 1))
  fi
  if [[ -f "$ENV_FILE" ]]; then
    ok ".env"
    if load_keystore_password && [[ -n "${KEYSTORE_PASSWORD:-}" ]]; then
      ok "KEYSTORE_PASSWORD set"
    else
      echo "${RED}✗${RESET} KEYSTORE_PASSWORD not set in $ENV_FILE"; fails=$((fails + 1))
    fi
  else
    echo "${RED}✗${RESET} missing: $ENV_FILE"; fails=$((fails + 1))
  fi

  step "SYNC STATUS"
  if command -v monad-status >/dev/null 2>&1; then
    local out status diff
    out="$(monad-status 2>/dev/null || true)"
    status="$(echo "$out" | grep -m1 'status:' | awk '{print $2}' || true)"
    diff="$(echo "$out" | grep -m1 'blockDifference:' | awk '{print $2}' || true)"
    if [[ "$status" == "in-sync" ]]; then
      ok "in-sync (block difference: ${diff:-0})"
    else
      echo "${RED}✗${RESET} node is ${status:-unknown} — must be in-sync before promotion"
      fails=$((fails + 1))
    fi
  else
    warn "cannot verify sync without monad-status"; warns=$((warns + 1))
  fi

  check_rpc

  step "KEY BACKUP FILES"
  local dir="$KEY_SOURCE_DIR" f ikm
  [[ -z "$dir" || "$dir" == "-" ]] && dir="$BACKUP_ROOT"
  for f in secp-backup bls-backup; do
    if [[ -f "$dir/$f" ]]; then
      ikm="$(extract_ikm_from_backup "$dir/$f")"
      if validate_ikm "$ikm" >/dev/null; then
        ok "$dir/$f (valid IKM format)"
      else
        warn "$dir/$f exists but contains no valid IKM"; warns=$((warns + 1))
      fi
    else
      warn "$dir/$f not found — manual IKM entry would be required"; warns=$((warns + 1))
    fi
  done
  ikm=""

  if [[ -f "$NODE_TOML" ]]; then
    verify_config_flags
  fi

  step "PLANNED ACTIONS (live run would do)"
  echo "  1. Back up this server's keys and config to $BACKUP_ROOT/failover-<timestamp>/"
  echo "  2. Import validator keys to staging files (id-secp.new / id-bls.new)"
  echo "  3. Set node_name, beneficiary, required config flags"
  echo "  4. Sign name record (seq_num = previous + 1) and patch node.toml"
  echo "  5. After confirming the old validator is stopped: stop services,"
  echo "     swap keys into place, restart as validator"
  echo "  6. Re-export fresh key backups from the live keys"

  echo
  bar
  if [[ "$fails" -gt 0 ]]; then
    echo "${RED}✗${RESET} ${BOLD}Preflight failed${RESET} — $fails blocking issue(s), $warns warning(s)."
    exit 1
  fi
  ok "${BOLD}Preflight passed${RESET} — $warns warning(s). This server is ready for a live run."
  echo
}

# ══════════════════════════════════════════════════════════
# CLI
# ══════════════════════════════════════════════════════════
usage() {
  echo "monad-failover v${VERSION} — promote a synced Monad full node to validator"
  echo
  echo "Usage:"
  echo "  ./monad-failover.sh [--backup-dir PATH] [--resume]"
  echo "  ./monad-failover.sh --dry-run"
  echo
  echo "Flags:"
  echo "  --dry-run     Read-only preflight: run every check, change nothing"
  echo "  --backup-dir  Directory containing secp-backup / bls-backup key files"
  echo "                (skips the interactive key-source prompt)"
  echo "  --resume      Continue from the last completed step"
  echo "  --version     Print version and exit"
  exit 0
}

RESUME=false
KEY_SOURCE_DIR=""
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)    DRY_RUN=true ;;
    --resume)     RESUME=true ;;
    --backup-dir) shift; KEY_SOURCE_DIR="${1:-}"; [[ -n "$KEY_SOURCE_DIR" ]] || die "--backup-dir requires a value" ;;
    --version)    echo "monad-failover v${VERSION}"; exit 0 ;;
    -h|--help|help) usage ;;
    *)            die "Unknown argument: $1. Run with --help for usage." ;;
  esac
  shift
done

# ── main ───────────────────────────────────────────────────
if $DRY_RUN; then
  mode_dry_run
  exit 0
fi

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
    if confirm_yn "  Start fresh?"; then
      clear_state
    else
      echo "  Use: ./monad-failover.sh --resume"
      exit 0
    fi
  fi
fi

clear 2>/dev/null || true
promote
