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

VERSION="1.9.0"

# ── paths (env-overridable for testing) ────────────────────
MONAD_HOME="${MONAD_HOME:-/home/monad}"
CONFIG_DIR="$MONAD_HOME/monad-bft/config"
NODE_TOML="$CONFIG_DIR/node.toml"
ENV_FILE="$MONAD_HOME/.env"
SECP_KEY="$CONFIG_DIR/id-secp"
BLS_KEY="$CONFIG_DIR/id-bls"
SECP_KEY_NEW="$CONFIG_DIR/id-secp.new"
BLS_KEY_NEW="$CONFIG_DIR/id-bls.new"
NODE_TOML_NEW="$CONFIG_DIR/node.toml.new"
BACKUP_ROOT="${BACKUP_ROOT:-/opt/monad/backup}"
LOG_DIR="${LOG_DIR:-/opt/monad/failover-logs}"
MONAD_SERVICES=(monad-bft monad-execution monad-rpc)

# ── ui helpers ─────────────────────────────────────────────
BOLD=$'\033[1m' DIM=$'\033[2m'
RED=$'\033[31m' GREEN=$'\033[32m' YELLOW=$'\033[33m' CYAN=$'\033[36m'
RESET=$'\033[0m'

header() {
  echo
  echo "┌───────────────────────────────────────────────────────────"
  printf '│  %b%-44s%b%12s\n' "$BOLD" "MONAD VALIDATOR FAILOVER" "$RESET" "v$VERSION"
  printf '│  %s · %s\n' "$(hostname)" "$(date '+%Y-%m-%d %H:%M:%S %Z')"
  echo "└───────────────────────────────────────────────────────────"
}

# Full-width phase banner with a step counter, e.g. "━━ [4/8] KEY IMPORT ━━..."
PHASES_TOTAL=8
phase() {
  local n="$1" title="$2"
  local ascii="-- [${n}/${PHASES_TOTAL}] ${title} "
  local fill=$(( 60 - ${#ascii} ))
  echo
  printf '%b━━ [%s/%s] %s ' "$CYAN" "$n" "$PHASES_TOTAL" "$title"
  if (( fill > 0 )); then printf '━%.0s' $(seq 1 "$fill"); fi
  printf '%b\n' "$RESET"
}

bar()      { echo "${DIM}──────────────────────────────────────────────${RESET}"; }
step()     { echo; echo "${CYAN}▸${RESET} ${BOLD}$*${RESET}"; }
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
  local prompt="$1" ans
  read -r -p "$(printf '  %b?%b %s (y/N) › ' "$CYAN" "$RESET" "$prompt")" ans
  case "${ans,,}" in y|yes) return 0 ;; *) return 1 ;; esac
}

# Styled input prompt: ask "label" VAR  →  "  ? label › "
ask() {
  local label="$1" __var="$2"
  read -r -p "$(printf '  %b?%b %s › ' "$CYAN" "$RESET" "$label")" "${__var?}"
}

# Read KEYSTORE_PASSWORD from .env WITHOUT sourcing it. .env is owned by the
# monad user; sourcing it as root would execute anything a compromised monad
# account placed there. We only ever need this one value.
load_keystore_password() {
  local line val
  line="$(grep -m1 '^KEYSTORE_PASSWORD=' "$ENV_FILE" 2>/dev/null || true)"
  [[ -n "$line" ]] || return 1
  val="${line#KEYSTORE_PASSWORD=}"
  # A .env saved with CRLF endings carries a trailing \r that would defeat
  # the quote-strip below and end up inside the password. Drop it first.
  val="${val%$'\r'}"
  if [[ "$val" == \'*\' ]]; then
    val="${val#\'}"; val="${val%\'}"
  elif [[ "$val" == \"*\" ]]; then
    val="${val#\"}"; val="${val%\"}"
  fi
  KEYSTORE_PASSWORD="$val"
}

valid_ipv4() {
  local ip="$1" o
  [[ "$ip" =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
  for o in "${BASH_REMATCH[@]:1}"; do
    (( 10#$o <= 255 )) || return 1
  done
}

# The public IPv4: the --public-ip override if given, otherwise detected over
# HTTPS and validated. Echoes the IP or nothing.
public_ip() {
  if [[ -n "${PUBLIC_IP:-}" ]]; then
    printf '%s' "$PUBLIC_IP"
    return
  fi
  local ip
  ip="$(curl -fsS4 --connect-timeout 10 --max-time 20 https://ifconfig.me 2>/dev/null | head -c 64 || true)"
  if valid_ipv4 "$ip"; then
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
# A resume run reads this state back and acts on it as root: it names the
# backup directory to restore from, the seq to sign, the IP to publish. So
# whoever can write it can steer that run. It therefore lives in a root-owned
# directory of its own, not under $MONAD_HOME, which belongs to the
# unprivileged monad account the node services run as.
STATE_DIR_DEFAULT="/var/lib/monad-failover"
LEGACY_STATE_DIR="$MONAD_HOME/.monad-failover"

# MF_STATE_DIR exists only for the sandboxed test suite, which runs unprivileged
# and sets MF_ALLOW_NONROOT. Honouring it in a real run would defeat the move:
# the state directory could be pointed straight back at a user-writable path.
# It is therefore accepted only when BOTH the process is not root AND the test
# escape hatch is set; a root run ignores it and dies rather than silently
# using $STATE_DIR_DEFAULT, so a stray MF_STATE_DIR can never be assumed honoured.
if [[ $EUID -ne 0 && -n "${MF_ALLOW_NONROOT:-}" ]]; then
  STATE_DIR="${MF_STATE_DIR:-$STATE_DIR_DEFAULT}"
else
  [[ -z "${MF_STATE_DIR:-}" ]] || die \
    "MF_STATE_DIR is only honoured by the unprivileged test suite." \
    "In a live (root) run the state directory is fixed at $STATE_DIR_DEFAULT so" \
    "it cannot be redirected to a location an unprivileged user controls."
  STATE_DIR="$STATE_DIR_DEFAULT"
fi
STATE_FILE="$STATE_DIR/state"

# Rewrite-then-rename instead of sed: values (paths, signatures) need no
# escaping this way, and the update is atomic. The temp file is created by
# mktemp inside the (root-only) state directory rather than at a predictable
# name, so it cannot be pre-staged as a symlink pointing somewhere else.
save_state() {
  local tmp
  tmp="$(mktemp "${STATE_DIR}/.state.XXXXXX")" || die "Could not write to $STATE_DIR"
  grep -v "^${1}=" "$STATE_FILE" 2>/dev/null > "$tmp" || true
  printf '%s=%s\n' "$1" "$2" >> "$tmp"
  chmod 600 "$tmp"
  mv -f "$tmp" "$STATE_FILE"
}

load_state() {
  grep "^${1}=" "$STATE_FILE" 2>/dev/null | cut -d '=' -f2- || true
}

clear_state() { rm -f "$STATE_FILE"; }

# Create or validate the state directory. A live run trusts what it reads back
# from here, so refuse anything an unprivileged user could have staged: a
# symlink standing in for the directory or the file, a directory owned by
# someone else, or a state file that is not a regular file.
secure_state_dir() {
  if [[ -L "$STATE_DIR" ]]; then
    die "$STATE_DIR is a symlink; refusing to use it." \
      "Remove it and re-run so the directory is created directly."
  fi

  # Create it ourselves at 0700 when absent; as root that also makes it
  # root-owned. An existing directory is NOT repaired and then trusted: if it
  # was ever group/other-writable, someone may have planted the state file we
  # would go on to act on. So an existing directory must ALREADY be exactly
  # 0700 (and, in a root run, root-owned); otherwise refuse and let the
  # operator remove and recreate it. Mode is checked in every run; ownership
  # only means something as root, where the state actually needs protecting.
  if [[ ! -d "$STATE_DIR" ]]; then
    (umask 077 && mkdir -p "$STATE_DIR") || die "Could not create $STATE_DIR"
  fi
  local dmode
  dmode="$(stat -c '%a' "$STATE_DIR" 2>/dev/null || true)"
  if [[ "$dmode" != "700" ]]; then
    die "$STATE_DIR has mode $dmode, not 700; refusing to use it." \
      "It may have been writable by another user, so its contents are not" \
      "trusted. Remove it and re-run:  rm -rf $STATE_DIR"
  fi
  if [[ $EUID -eq 0 ]]; then
    local downer
    downer="$(stat -c '%u' "$STATE_DIR" 2>/dev/null || true)"
    if [[ "$downer" != "0" ]]; then
      die "$STATE_DIR is owned by uid $downer, not root; refusing to use it." \
        "Resume state must not be writable by the monad service account." \
        "Remove it and re-run:  rm -rf $STATE_DIR"
    fi
  fi

  if [[ -L "$STATE_FILE" ]]; then
    die "$STATE_FILE is a symlink; refusing to read or write through it." \
      "Remove it. If a run was interrupted, restore this node from" \
      "$BACKUP_ROOT and start a fresh run."
  fi
  if [[ -e "$STATE_FILE" ]]; then
    [[ -f "$STATE_FILE" ]] || die "$STATE_FILE is not a regular file; refusing to use it."
    if [[ $EUID -eq 0 ]]; then
      local fowner
      fowner="$(stat -c '%u' "$STATE_FILE" 2>/dev/null || true)"
      [[ "$fowner" == "0" ]] || die \
        "$STATE_FILE is owned by uid $fowner, not root; refusing to use it." \
        "Remove it and start a fresh run."
    fi
    # Tighten going forward (defence in depth; the 0700 root dir already keeps
    # everyone else out). This narrows, it never widens, so it cannot make a
    # loose file trusted.
    chmod 600 "$STATE_FILE" 2>/dev/null || true
  fi
}

# Cross-field consistency, enforced before BOTH the resume path and the
# fresh-run decision so a tampered or truncated state file slips past neither.
# Field shapes are re-checked here because this runs ahead of the per-field
# resume validation.
validate_state_consistency() {
  [[ -f "$STATE_FILE" ]] || return 0
  local ls cs s1 s2 s3
  ls="$(load_state "last_step")"
  cs="$(load_state "cutover_started")"
  s1="$(load_state "staged_secp_sha")"
  s2="$(load_state "staged_bls_sha")"
  s3="$(load_state "staged_toml_sha")"

  check_state_field "cutover_started" "$cs" '^1?$'
  check_state_field "staged_secp_sha" "$s1" '^([0-9a-f]{64})?$'
  check_state_field "staged_bls_sha"  "$s2" '^([0-9a-f]{64})?$'
  check_state_field "staged_toml_sha" "$s3" '^([0-9a-f]{64})?$'
  [[ -z "$ls" ]] || check_state_field "last_step" "$ls" '^[1-8]$'

  # A cutover in progress must carry the three staged checksums (saved just
  # before the flag) and sit at step 6-8; without them a resumed cutover cannot
  # tell a completed swap from a partial one.
  if [[ "$cs" == "1" ]]; then
    [[ "$ls" =~ ^[678]$ ]] || die \
      "State file is inconsistent: cutover is marked started but last_step is '$ls'." \
      "Refusing to act on it." \
      "Restore this node from $BACKUP_ROOT, remove $STATE_FILE, then start fresh."
    [[ -n "$s1" && -n "$s2" && -n "$s3" ]] || die \
      "State file is inconsistent: cutover is marked started but a staged checksum is missing." \
      "Refusing to act on it." \
      "Restore this node from $BACKUP_ROOT, remove $STATE_FILE, then start fresh."
  fi

  # Reaching step 7 means the cutover ran; the flag must say so. If it does not,
  # offering "start fresh" would re-snapshot a swapped identity as this node's
  # own, so refuse and require --resume (or a manual restore) instead.
  if [[ -n "$ls" && "$ls" -ge 7 && "$cs" != "1" ]]; then
    die \
      "State file is inconsistent: it reached step $ls but cutover is not marked started." \
      "Refusing to act on it." \
      "Finish with --resume only if the cutover truly completed; otherwise restore" \
      "from $BACKUP_ROOT, remove $STATE_FILE, and start fresh."
  fi
}

# Older versions kept state under $MONAD_HOME, which the monad account can
# write. That content is not trusted and is deliberately not migrated: a
# resume driven by it would be a resume driven by whatever wrote it.
refuse_legacy_state() {
  local f found=""
  for f in "$LEGACY_STATE_DIR/state" "$LEGACY_STATE_DIR/promote/state"; do
    if [[ -e "$f" || -L "$f" ]]; then found="$f"; break; fi
  done
  [[ -n "$found" ]] || return 0
  die "Found state from an older version at $found." \
    "That path is under $MONAD_HOME and writable by the monad service" \
    "account, so it is not trusted and is not migrated automatically." \
    "Review it, then remove the directory:  rm -rf $LEGACY_STATE_DIR" \
    "If a previous run was interrupted, restore this node from $BACKUP_ROOT" \
    "and start a fresh run rather than resuming from it."
}

# Reject a state value whose shape is wrong. Every field below reaches a
# config file, a signed name record, a path or a shell expansion, so the
# allowlists are deliberately narrow: no whitespace, no shell or arithmetic
# metacharacters.
check_state_field() {
  local key="$1" val="$2" re="$3"
  [[ "$val" =~ $re ]] || die \
    "State file is corrupt or was tampered with: '$key' holds an unexpected value." \
    "Refusing to resume from it." \
    "Restore this node from $BACKUP_ROOT if a run was interrupted, remove" \
    "$STATE_FILE, then start a fresh run."
}

completed_step() {
  local c; c="$(load_state "last_step")"
  [[ "$c" =~ ^[1-8]$ ]] && [[ "$c" -ge "$1" ]]
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
    confirm_yn "continue without sync check?" || die "Aborted."
  fi
}

# Official Monad RPC ports (8080/8081) plus commonly exposed EVM RPC ports.
RPC_PORTS=(8080 8081 8545 8546 9545 9546 18545 18546)

check_rpc() {
  step "RPC EXPOSURE CHECK"
  if ! command -v ss >/dev/null 2>&1; then
    warn "ss not found — cannot check RPC exposure."
    echo "  Verify manually that none of these ports listen publicly: ${RPC_PORTS[*]}"
    return 0
  fi
  local listeners exposed=()
  listeners="$(ss -ltn 2>/dev/null | awk '{print $4}' || true)"
  for port in "${RPC_PORTS[@]}"; do
    if echo "$listeners" | grep -qE "^(0\.0\.0\.0|\*|\[::\]):${port}$"; then
      exposed+=("$port")
    fi
  done
  if [[ ${#exposed[@]} -gt 0 ]]; then
    warn "RPC ports listening on all interfaces: ${exposed[*]}"
    echo "  Validators should not expose RPC publicly. If a firewall (ufw etc.)"
    echo "  already blocks these ports from outside, you are fine as is."
    echo "  Otherwise bind them to localhost or block them now."
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
  echo
  warn "This will ${BOLD}promote this full node to validator${RESET}."
  echo "  Hostname:  ${BOLD}$(hostname)${RESET}"

  local ip
  ip="$(public_ip)"
  [[ -n "$ip" ]] && echo "  Public IP: ${BOLD}${ip}${RESET}"

  echo
  confirm_yn "is this the correct target host?" || die "Aborted."
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
  # Output is dropped on purpose: every live run is recorded to a log file,
  # and a CLI error path that echoes its arguments would persist the IKM or
  # the keystore password to disk. Same handling as recover/export.
  monad-keystore import \
    --ikm "$ikm" \
    --keystore-path "$keypath" \
    --password "$KEYSTORE_PASSWORD" >/dev/null 2>&1 \
    || die "monad-keystore import failed for $keypath." \
           "(Its output is suppressed so secrets can never reach the run log.)" \
           "Check the keystore password in $ENV_FILE and the IKM source, then re-run."
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
  local toml="${1:-$NODE_TOML}"
  step "VERIFY CONFIG FLAGS"
  local missing=""
  grep -q '^enable_publisher = true' "$toml" || missing="${missing} enable_publisher"
  grep -q '^enable_client = true' "$toml"    || missing="${missing} enable_client"
  grep -q '^expand_to_group = true' "$toml"  || missing="${missing} expand_to_group"

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

  # No --node-config on purpose: with it, the signer reads the seq from the
  # CURRENT node.toml (stale on a fresh full node) and ignores the argument,
  # signing a stale seq — the exact failure seen in a live migration. Without
  # it the signer takes the seq from the argument and the pubkey from the
  # keystore, matching the official install-guide invocation. One seq source.
  step "SIGN NAME RECORD (seq $seq)"
  # stderr is dropped for the same reason as in import_staged_key: this call
  # carries the keystore password on argv, and an error path echoing it would
  # persist the secret to the run log.
  local sign_out
  if ! sign_out="$(monad-sign-name-record \
    --ip "$ip" \
    --tcp-port 8000 \
    --udp-port 8000 \
    --authenticated-udp-port 8001 \
    --self-record-seq-num "$seq" \
    --keystore-path "$keypath" \
    --password "$KEYSTORE_PASSWORD" 2>/dev/null)"; then
    die "monad-sign-name-record failed." \
      "(Its error output is suppressed so secrets can never reach the run log.)" \
      "Check the keystore password in $ENV_FILE and the monad version, then re-run."
  fi

  # The signature is bound to the values the signer EMITS, so the output is
  # the single source of truth for all three patched fields.
  SELF_ADDRESS="$(echo "$sign_out" | grep '^self_address ' | cut -d '"' -f2 || true)"
  SELF_SIG="$(echo "$sign_out" | grep '^self_name_record_sig ' | cut -d '"' -f2 || true)"
  SELF_SEQ="$(echo "$sign_out" | grep '^self_record_seq_num ' | awk '{print $NF}' || true)"

  [[ -n "$SELF_ADDRESS" ]]      || die "Failed to parse self_address from signer output"
  [[ -n "$SELF_SIG" ]]          || die "Failed to parse self_name_record_sig from signer output"
  [[ "$SELF_SEQ" =~ ^[0-9]+$ ]] || die "Failed to parse self_record_seq_num from signer output"

  # Guard against signer/version drift: emitting LESS than requested would
  # re-create the stale-seq ghost-node failure — hard stop. Emitting more
  # (a +1-style version) is monotonic-safe; warn and continue.
  if [[ "$SELF_SEQ" -lt "$seq" ]]; then
    die "Signer emitted seq $SELF_SEQ, lower than the requested $seq." \
      "A stale seq would be rejected by peers. Check the monad version" \
      "and re-run; nothing has been written."
  elif [[ "$SELF_SEQ" -ne "$seq" ]]; then
    warn "Signer emitted seq $SELF_SEQ (requested $seq) — this monad version increments it."
    echo "  Safe to continue: the signature matches the emitted value."
  fi
  ok "Name record signed (seq $SELF_SEQ)"

  step "PATCH node.toml"
  local esc_addr esc_sig
  esc_addr="$(sed_escape_replacement "$SELF_ADDRESS")"
  esc_sig="$(sed_escape_replacement "$SELF_SIG")"
  sed -i "s|^self_address.*|self_address = \"$esc_addr\"|" "$toml"
  sed -i "s|^self_record_seq_num.*|self_record_seq_num = $SELF_SEQ|" "$toml"
  sed -i "s|^self_name_record_sig.*|self_name_record_sig = \"$esc_sig\"|" "$toml"

  grep -qF "self_address = \"$SELF_ADDRESS\"" "$toml"      || die "Failed to write self_address"
  grep -qF "self_record_seq_num = $SELF_SEQ" "$toml"       || die "Failed to write self_record_seq_num"
  fix_ownership
  ok "node.toml patched and verified"
}

backup_config() {
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
# Returns nonzero on export failure so the caller keeps the resume state —
# otherwise the printed --resume advice would find nothing to resume.
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
    return 1
  fi
}

# Hard health gate after cutover: `systemctl start` returning success does
# not mean the services survived their first seconds. Wait, then require
# every unit to be active before the run may call itself complete.
# (MF_HEALTH_WAIT exists solely so the test suite can skip the wait.)
post_verify() {
  step "POST-CUTOVER VERIFICATION"
  sleep "${MF_HEALTH_WAIT:-5}"
  local svc
  for svc in "${MONAD_SERVICES[@]}"; do
    systemctl is-active --quiet "$svc" 2>/dev/null || die \
      "$svc is not active after cutover." \
      "Check:  journalctl -xeu $svc" \
      "Start:  systemctl start ${MONAD_SERVICES[*]}" \
      "Finish: ./monad-failover.sh --resume"
  done
  ok "All services active"
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
  resp="$(curl -fsS --connect-timeout 10 --max-time 30 "$url" 2>/dev/null | head -c 65536 || true)"

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

# ── cutover helpers ────────────────────────────────────────
file_sha() { sha256sum "$1" 2>/dev/null | awk '{print $1}' || true; }

# True if this staged file can be placed: it either still exists, or a
# previous cutover attempt already moved it into place (the live file matches
# the checksum recorded just before that attempt). This is what lets --resume
# finish an interrupted cutover instead of wedging on a consumed staging file.
staged_ready() {
  local staged="$1" live="$2" state_key="$3"
  [[ -f "$staged" ]] && return 0
  local want
  want="$(load_state "$state_key")"
  [[ -n "$want" && "$(file_sha "$live")" == "$want" ]]
}

# Move a staged file into place; a no-op if a previous attempt already did
# (staged_ready has verified the live file's checksum before services stopped).
place_staged() {
  local staged="$1" live="$2" label="$3"
  if [[ -f "$staged" ]]; then
    mv -f "$staged" "$live"
  else
    ok "$label already in place from a previous cutover attempt"
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

# Record every live run to a log file. Secrets never appear in the output
# (hidden input, no echo), so the log is safe to keep; it is the operator's
# only record of what happened during a migration.
start_logging() {
  mkdir -p "$LOG_DIR"
  chmod 700 "$LOG_DIR" 2>/dev/null || true
  LOG_FILE="$LOG_DIR/failover-$(date +%Y%m%d-%H%M%S).log"
  exec > >(tee -a "$LOG_FILE") 2>&1
  echo "${DIM}(logging this run to $LOG_FILE)${RESET}"
}

# ══════════════════════════════════════════════════════════
# PROMOTE — full node → validator
# ══════════════════════════════════════════════════════════
promote() {
  start_logging
  header

  need_cmd curl; need_cmd systemctl; need_cmd sed; need_cmd sha256sum
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
      # Validate before the arithmetic below, not after: $(( )) evaluates an
      # array subscript, and a subscript runs command substitution, so an
      # unchecked value here would execute as root. The range is closed, not
      # just "digits": an out-of-range value like 999 would satisfy every
      # completed_step check and skip the whole migration to a fake "complete".
      check_state_field "last_step" "$last" '^[1-8]$'
      ok "Resuming from step $((last + 1))"
      NETWORK="$(load_state "network")"
      NEW_SEQ="$(load_state "new_seq")"
      SECP_PUB="$(load_state "secp_pub")"
      BLS_PUB="$(load_state "bls_pub")"
      IP="$(load_state "ip")"
      SELF_ADDRESS="$(load_state "self_address")"
      SELF_SIG="$(load_state "self_sig")"
      SELF_SEQ="$(load_state "self_seq")"
      BENEFICIARY="$(load_state "beneficiary")"
      BACKUP_DIR="$(load_state "backup_dir")"

      CUTOVER_STARTED="$(load_state "cutover_started")"
      STAGED_SECP_SHA="$(load_state "staged_secp_sha")"
      STAGED_BLS_SHA="$(load_state "staged_bls_sha")"
      STAGED_TOML_SHA="$(load_state "staged_toml_sha")"

      # Shapes are narrowed to the real format of each field, not a loose
      # charset: every one of these reaches a config file, a signed name
      # record, a URL or a shell expansion. Empty is allowed only where the
      # field is genuinely optional or not yet set at this step; the presence
      # checks below then enforce what THIS step must have.
      check_state_field "network"      "$NETWORK"      '^(mainnet|testnet)?$'
      check_state_field "new_seq"      "$NEW_SEQ"      '^[0-9]*$'
      check_state_field "self_seq"     "$SELF_SEQ"     '^[0-9]*$'
      check_state_field "secp_pub"     "$SECP_PUB"     '^[0-9A-Za-z]*$'
      check_state_field "bls_pub"      "$BLS_PUB"      '^[0-9A-Za-z]*$'
      check_state_field "self_sig"     "$SELF_SIG"     '^[0-9A-Za-z]*$'
      check_state_field "self_address" "$SELF_ADDRESS" '^([0-9.]+:[0-9]+)?$'
      check_state_field "beneficiary"  "$BENEFICIARY"  '^(0x[0-9A-Fa-f]{40})?$'
      check_state_field "cutover_started"  "$CUTOVER_STARTED"  '^1?$'
      check_state_field "staged_secp_sha"  "$STAGED_SECP_SHA"  '^[0-9a-f]{64}$|^$'
      check_state_field "staged_bls_sha"   "$STAGED_BLS_SHA"   '^[0-9a-f]{64}$|^$'
      check_state_field "staged_toml_sha"  "$STAGED_TOML_SHA"  '^[0-9a-f]{64}$|^$'
      check_state_field "ip" "$IP" '^([0-9]{1,3}(\.[0-9]{1,3}){3})?$'
      if [[ -n "$IP" ]] && ! valid_ipv4 "$IP"; then
        die "State file is corrupt or was tampered with: 'ip' is not a valid IPv4 address." \
          "Refusing to resume from it." \
          "Remove $STATE_FILE and start a fresh run."
      fi

      # backup_dir is not a restore source: nothing is copied FROM it. It is
      # the destination the pre-cutover backup was written to, and on resume it
      # is only shown to the operator as "restore this node's identity from
      # here" if a later step fails. Validate it anyway: a tampered value would
      # otherwise send the operator to an attacker-chosen path. String compare,
      # not a regex, because BACKUP_ROOT is a path whose dots must stay literal.
      check_state_field "backup_dir" "$BACKUP_DIR" '^[A-Za-z0-9._/-]*$'
      if [[ -n "$BACKUP_DIR" ]]; then
        if [[ "$BACKUP_DIR" == *".."* || "$BACKUP_DIR" != "$BACKUP_ROOT"/* ]]; then
          die "State file is corrupt or was tampered with: 'backup_dir' is not under $BACKUP_ROOT." \
            "Refusing to resume: recovery messages would point at that path." \
            "Remove $STATE_FILE and start a fresh run."
        fi
      fi

      # Presence is step-dependent. Each field is written by the step named in
      # its comment (save_state calls), so once that step is behind us the
      # field must be there; a blank one means the state file was truncated or
      # tampered with, and resuming past it would sign or publish an empty value.
      require_state() {
        [[ -n "$2" ]] || die \
          "State file is incomplete: '$1' is empty but the run had reached step $last." \
          "Refusing to resume from a partial state file." \
          "Restore this node from $BACKUP_ROOT if a run was interrupted, remove" \
          "$STATE_FILE, then start a fresh run."
      }
      [[ "$last" -ge 2 ]] && require_state "network"      "$NETWORK"
      [[ "$last" -ge 4 ]] && require_state "secp_pub"     "$SECP_PUB"
      [[ "$last" -ge 4 ]] && require_state "bls_pub"      "$BLS_PUB"
      [[ "$last" -ge 5 ]] && require_state "new_seq"      "$NEW_SEQ"
      [[ "$last" -ge 6 ]] && require_state "ip"           "$IP"
      [[ "$last" -ge 6 ]] && require_state "self_address" "$SELF_ADDRESS"
      [[ "$last" -ge 6 ]] && require_state "self_sig"     "$SELF_SIG"
      [[ "$last" -ge 6 ]] && require_state "self_seq"     "$SELF_SEQ"
    fi
  fi

  # ── 1. Sync + RPC ──
  if ! $RESUME || ! completed_step 1; then
    phase 1 "PREFLIGHT"
    check_sync
    check_rpc
    save_state "last_step" "1"
  fi

  # ── 2. Network + location guard ──
  if ! $RESUME || ! completed_step 2; then
    phase 2 "NETWORK & HOST"
    detect_network
    run_location_guard
    save_state "network" "$NETWORK"
    save_state "last_step" "2"
  fi

  # ── 3. Backup this server's own identity ──
  if ! $RESUME || ! completed_step 3; then
    phase 3 "BACKUP CURRENT CONFIG"
    backup_config
    save_state "last_step" "3"
  fi

  # ── 4. Import validator keys (staging — live keys untouched) ──
  if ! $RESUME || ! completed_step 4; then
    rm -f "$SECP_KEY_NEW" "$BLS_KEY_NEW" "$NODE_TOML_NEW"

    phase 4 "VALIDATOR KEY IMPORT"
    echo "  Keys are imported to staging files (id-secp.new / id-bls.new)."
    echo "  Live keys remain untouched until cutover."
    echo

    local SECP_IKM="" BLS_IKM=""

    if [[ -z "$KEY_SOURCE_DIR" ]]; then
      echo "    1) Key backup files (secp-backup / bls-backup) — ${GREEN}recommended${RESET}"
      echo "       Works even when the old server is unreachable."
      echo "    2) Paste IKM hex values manually (hidden input)"
      echo
      local key_choice
      ask "select (1/2)" key_choice
      case "$key_choice" in
        1) ask "backup directory [$BACKUP_ROOT]" KEY_SOURCE_DIR
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
      echo "  Paste the validator IKM hex values. Input is hidden."
      echo
      read -r -s -p "$(printf '  %b?%b SECP IKM_HEX › ' "$CYAN" "$RESET")" SECP_IKM; echo
      SECP_IKM="$(validate_ikm "$SECP_IKM")" || die "SECP IKM must be 64 hex characters"
      read -r -s -p "$(printf '  %b?%b BLS  IKM_HEX › ' "$CYAN" "$RESET")" BLS_IKM; echo
      BLS_IKM="$(validate_ikm "$BLS_IKM")" || die "BLS IKM must be 64 hex characters"
    fi

    step "Importing SECP key (staging)"
    import_staged_key "$SECP_IKM" "$SECP_KEY_NEW"
    ok "SECP key imported to id-secp.new"

    step "Importing BLS key (staging)"
    import_staged_key "$BLS_IKM" "$BLS_KEY_NEW"
    ok "BLS key imported to id-bls.new"
    SECP_IKM="" BLS_IKM=""

    SECP_PUB="$(recover_pubkey "$SECP_KEY_NEW" secp)"
    BLS_PUB="$(recover_pubkey "$BLS_KEY_NEW" bls)"

    [[ -n "$SECP_PUB" ]] || die "Could not recover SECP public key"
    [[ -n "$BLS_PUB" ]]  || die "Could not recover BLS public key"

    echo
    echo "  SECP: ${BOLD}${SECP_PUB:0:46}...${RESET}"
    echo "  BLS:  ${BOLD}${BLS_PUB:0:46}...${RESET}"
    echo
    confirm_yn "do these match your validator keys?" || die "Key mismatch — aborting."
    ok "Keys verified"

    save_state "secp_pub" "$SECP_PUB"
    save_state "bls_pub" "$BLS_PUB"
    save_state "last_step" "4"
  fi

  # ── 5. Beneficiary + seq + config flags (all on a staging copy) ──
  if ! $RESUME || ! completed_step 5; then
    phase 5 "CONFIGURE VALIDATOR"
    echo "  All changes go to a staging copy (node.toml.new)."
    echo "  The live config is untouched until cutover."

    # Like the keys: never touch the live node.toml before cutover. An abort
    # at the STOPPED gate must leave a fully unmodified full node behind.
    cp -a "$NODE_TOML" "$NODE_TOML_NEW"

    echo
    echo "${BOLD}BENEFICIARY${RESET}"
    echo "Enter the beneficiary address from the old validator's node.toml"
    ask "beneficiary" BENEFICIARY
    if [[ -n "$BENEFICIARY" ]]; then
      [[ "$BENEFICIARY" =~ ^0x[0-9a-fA-F]{40}$ ]] \
        || die "beneficiary must be a 0x-prefixed 40-hex-character address"
      set_toml_value "$NODE_TOML_NEW" "beneficiary" "\"$BENEFICIARY\""
      ok "Beneficiary: $BENEFICIARY"
    else
      warn "No beneficiary entered — check node.toml manually after promotion."
    fi

    echo
    echo "${BOLD}NODE NAME${RESET}"
    echo "Per the migration docs, this node should take over the old validator's"
    echo "node_name during migration. Leave empty to keep the current name."
    ask "node_name" NODE_NAME
    if [[ -n "$NODE_NAME" ]]; then
      [[ "$NODE_NAME" =~ ^[A-Za-z0-9._-]{1,64}$ ]] \
        || die "node_name may contain only letters, digits, dot, dash, underscore (max 64)"
      set_toml_value "$NODE_TOML_NEW" "node_name" "\"$NODE_NAME\""
      ok "node_name: $NODE_NAME"
    else
      ok "node_name unchanged"
    fi

    echo
    echo "${BOLD}SEQ NUM${RESET}"
    echo "Enter the self_record_seq_num to use for THIS migration — the final value,"
    echo "no math is done on it. It must be higher than the last value this validator"
    echo "identity ever used. That number is NOT on this machine: check your records"
    echo "or the old validator's node.toml (last was 7 → enter 8; never migrated → 1)."
    echo "Unsure? Enter a value you are certain is higher — gaps are harmless."
    ask "new seq_num" NEW_SEQ
    [[ "$NEW_SEQ" =~ ^[1-9][0-9]*$ ]] || die "Must be a positive number"
    ok "seq_num for this migration: $NEW_SEQ"

    set_toml_value "$NODE_TOML_NEW" "enable_publisher" "true"  "fullnode_raptorcast"
    set_toml_value "$NODE_TOML_NEW" "enable_client"    "true"  "fullnode_raptorcast"
    set_toml_value "$NODE_TOML_NEW" "expand_to_group"  "true"  "statesync"
    verify_config_flags "$NODE_TOML_NEW"

    save_state "beneficiary" "${BENEFICIARY:-}"
    save_state "new_seq" "$NEW_SEQ"
    save_state "last_step" "5"
  fi

  # ── 6. Sign name record + patch (staging key, staging config) ──
  if ! $RESUME || ! completed_step 6; then
    phase 6 "SIGN NAME RECORD"
    [[ -f "$NODE_TOML_NEW" ]] || die \
      "Staging config (node.toml.new) is missing." \
      "Start a fresh run so the configure step re-creates it."
    detect_ip
    sign_and_patch "$NODE_TOML_NEW" "$IP" "$NEW_SEQ" "$SECP_KEY_NEW"

    save_state "ip" "$IP"
    save_state "self_address" "$SELF_ADDRESS"
    save_state "self_sig" "$SELF_SIG"
    save_state "self_seq" "$SELF_SEQ"
    save_state "last_step" "6"
  fi

  # ── 7. Confirm old validator stopped + cutover ──
  if ! $RESUME || ! completed_step 7; then
    phase 7 "CUTOVER"
    echo
    echo "┌─ PROMOTION SUMMARY ────────────────────────────────────────"
    printf '│  %-12s %s\n' "hostname"    "$(hostname)"
    printf '│  %-12s %s\n' "network"     "$NETWORK"
    printf '│  %-12s %s\n' "address"     "$SELF_ADDRESS"
    printf '│  %-12s %s\n' "seq_num"     "${SELF_SEQ:-$NEW_SEQ}"
    printf '│  %-12s %s\n' "beneficiary" "${BENEFICIARY:-not set}"
    printf '│  %-12s %s\n' "secp"        "${SECP_PUB:0:24}..."
    printf '│  %-12s %s\n' "bls"         "${BLS_PUB:0:24}..."
    echo "└────────────────────────────────────────────────────────────"
    echo

    warn "The old validator MUST be ${BOLD}stopped or fully offline${RESET} before cutover."
    echo "  Running two nodes with the same keys corrupts this validator's"
    echo "  consensus participation and name record."
    echo
    echo "  If the old server is reachable, stop it now:"
    echo "      ${BOLD}systemctl stop monad-bft monad-execution monad-rpc${RESET}"
    echo
    local confirm_stopped
    ask "type STOPPED to confirm" confirm_stopped
    [[ "$confirm_stopped" == "STOPPED" ]] || die "Not confirmed — aborting before cutover."
    ok "Old validator confirmed stopped or offline"

    echo
    warn "${BOLD}POINT OF NO RETURN${RESET}"
    echo "  The next step stops services, swaps in the validator keys, and starts."
    echo "  After this the old validator MUST NOT be restarted with the same keys."
    echo
    confirm_yn "proceed with cutover?" || die "Aborted."

    # Record checksums of what is about to be placed. If this cutover is
    # interrupted mid-swap, a re-run uses these to recognize the files that
    # already made it into place and only moves the rest — --resume can always
    # finish an interrupted cutover.
    [[ -f "$SECP_KEY_NEW"  ]] && save_state "staged_secp_sha" "$(file_sha "$SECP_KEY_NEW")"
    [[ -f "$BLS_KEY_NEW"   ]] && save_state "staged_bls_sha"  "$(file_sha "$BLS_KEY_NEW")"
    [[ -f "$NODE_TOML_NEW" ]] && save_state "staged_toml_sha" "$(file_sha "$NODE_TOML_NEW")"

    # Every staging file must be present, or verifiably already placed by a
    # previous attempt, before we touch services — never leave a half-swapped
    # identity or a key/config mismatch.
    if ! { staged_ready "$SECP_KEY_NEW" "$SECP_KEY" staged_secp_sha \
        && staged_ready "$BLS_KEY_NEW" "$BLS_KEY" staged_bls_sha \
        && staged_ready "$NODE_TOML_NEW" "$NODE_TOML" staged_toml_sha; }; then
      die "Staging files are missing — refusing to stop services." \
        "Start a fresh run so the import and configure steps re-create them."
    fi

    # From here on the run mutates the live node: mark it, so a later run
    # without --resume refuses to start fresh over a half-swapped identity.
    save_state "cutover_started" "1"

    stop_monad_services

    place_staged "$SECP_KEY_NEW" "$SECP_KEY" "SECP key" || die \
      "Could not place the SECP key. Services are stopped; nothing has changed." \
      "Fix the cause, then continue with: ./monad-failover.sh --resume"
    if ! place_staged "$BLS_KEY_NEW" "$BLS_KEY" "BLS key"; then
      die "CRITICAL: the SECP key was placed but the BLS key was not." \
        "This node now has a mismatched identity. Do NOT start the services." \
        "Fix the cause, then continue with: ./monad-failover.sh --resume" \
        "(it finishes placing the remaining files). To roll back instead," \
        "restore this node's previous identity from: ${BACKUP_DIR:-$BACKUP_ROOT}"
    fi
    if ! place_staged "$NODE_TOML_NEW" "$NODE_TOML" "node.toml"; then
      die "CRITICAL: the keys were placed but node.toml was not." \
        "Do NOT start the services with this key/config mismatch." \
        "Fix the cause, then continue with: ./monad-failover.sh --resume" \
        "(it finishes placing node.toml). To roll back instead, restore" \
        "this node's previous identity from: ${BACKUP_DIR:-$BACKUP_ROOT}"
    fi
    fix_ownership

    # The swap is complete: record it BEFORE starting services, so a crash or
    # start failure past this point resumes into verification, never back
    # into the (now impossible) swap.
    save_state "last_step" "7"

    systemctl enable "${MONAD_SERVICES[@]}" 2>/dev/null || true
    if ! systemctl start "${MONAD_SERVICES[@]}"; then
      die "The validator keys are in place, but the services failed to start." \
        "The swap is done — do NOT re-run the cutover." \
        "Diagnose: journalctl -xeu monad-bft" \
        "Start when fixed: systemctl start ${MONAD_SERVICES[*]}" \
        "Then finish up:  ./monad-failover.sh --resume"
    fi
    ok "Services started"
  fi

  # ── 8. Verify + refresh key backups ──
  if ! $RESUME || ! completed_step 8; then
    phase 8 "VERIFY"
    post_verify
    check_validator_api
    if ! refresh_key_backups; then
      die "Key backup export failed after an otherwise successful promotion." \
        "The validator itself is live — nothing else is wrong. Previous backup" \
        "copies are preserved as *.bak in $BACKUP_ROOT." \
        "Retry just this export with: ./monad-failover.sh --resume"
    fi
    save_state "last_step" "8"
  fi

  # ── done ──
  rm -f "$SECP_KEY_NEW" "$BLS_KEY_NEW" "$NODE_TOML_NEW" 2>/dev/null || true
  clear_state
  echo
  echo "${GREEN}════════════════════════════════════════════════════════════${RESET}"
  echo "   ${GREEN}✔${RESET}  ${BOLD}VALIDATOR PROMOTION COMPLETE${RESET}"
  echo "${GREEN}════════════════════════════════════════════════════════════${RESET}"

  echo
  echo "${BOLD}NODE STATUS${RESET}"
  echo "journalctl -fu monad-bft"

  echo
  echo "${BOLD}VALIDATOR EVENTS${RESET}"
  echo "journalctl -u monad-ledger-tail -o cat -f | grep -i \"${SECP_PUB}\""

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
  for c in curl systemctl sed sha256sum monad-keystore monad-sign-name-record; do
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
  echo "  3. Set node_name, beneficiary and config flags on a staging copy (node.toml.new)"
  echo "  4. Sign the name record with the seq_num you enter (used verbatim)"
  echo "     and patch the staged node.toml.new"
  echo "  5. After confirming the old validator is stopped: stop services,"
  echo "     swap the staged keys and config into place (resumable if"
  echo "     interrupted mid-swap), restart as validator"
  echo "  6. Verify every service is active, then re-export fresh key backups"

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
  echo "  --public-ip   Use this IPv4 in the name record instead of auto-detection"
  echo "  --resume      Continue from the last completed step"
  echo "  --version     Print version and exit"
  exit 0
}

RESUME=false
KEY_SOURCE_DIR=""
PUBLIC_IP=""
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)    DRY_RUN=true ;;
    --resume)     RESUME=true ;;
    --backup-dir) shift; KEY_SOURCE_DIR="${1:-}"; [[ -n "$KEY_SOURCE_DIR" ]] || die "--backup-dir requires a value" ;;
    --public-ip)  shift; PUBLIC_IP="${1:-}"; valid_ipv4 "$PUBLIC_IP" || die "--public-ip must be a valid IPv4 address" ;;
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

# Live runs manage systemd services and root-owned key files.
# (MF_ALLOW_NONROOT exists solely for the sandboxed test suite.)
[[ $EUID -eq 0 || -n "${MF_ALLOW_NONROOT:-}" ]] \
  || die "This script must run as root."

refuse_legacy_state
secure_state_dir
validate_state_consistency

if ! $RESUME && [[ -f "$STATE_FILE" ]]; then
  _last="$(load_state "last_step")"
  if [[ -n "$_last" ]]; then
    check_state_field "last_step" "$_last" '^[1-8]$'
    # Once a cutover has begun, "start fresh" is no longer safe: it would
    # re-snapshot (and re-reference) a possibly half-swapped identity as this
    # node's own. The interrupted run must be finished with --resume instead.
    if [[ "$(load_state "cutover_started")" == "1" ]]; then
      die "A previous run reached cutover — starting fresh is not safe now." \
        "Finish the interrupted run instead: ./monad-failover.sh --resume" \
        "(Only if you have manually restored this node and are sure, delete" \
        "$STATE_FILE to allow a fresh run.)"
    fi
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
