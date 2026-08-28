#!/usr/bin/env bats
# End-to-end tests for monad-failover.sh, run against mock Monad binaries.
# No network, no systemd, no real keys — everything happens in a tmpdir.

REPO_ROOT="$(cd "$(dirname "$BATS_TEST_FILENAME")/.." && pwd)"
SCRIPT="$REPO_ROOT/monad-failover.sh"

SECP_IKM="1111111111111111111111111111111111111111111111111111111111111111"
BLS_IKM="2222222222222222222222222222222222222222222222222222222222222222"

setup() {
  export MONAD_HOME="$BATS_TEST_TMPDIR/home/monad"
  export BACKUP_ROOT="$BATS_TEST_TMPDIR/opt/monad/backup"
  export LOG_DIR="$BATS_TEST_TMPDIR/opt/monad/failover-logs"
  # The real run pins state to /var/lib/monad-failover; MF_STATE_DIR is the
  # test-only override, honoured solely because MF_ALLOW_NONROOT is set below.
  export MF_STATE_DIR="$BATS_TEST_TMPDIR/var/lib/monad-failover"
  export MOCK_LOG="$BATS_TEST_TMPDIR/mock.log"
  export PATH="$REPO_ROOT/tests/mocks:$PATH"
  export MF_ALLOW_NONROOT=1
  export MF_HEALTH_WAIT=0
  mkdir -p "$MONAD_HOME/monad-bft/config" "$BACKUP_ROOT"
  touch "$MOCK_LOG"
}

# Build a healthy full-node environment: config, .env, live full-node keys,
# and valid validator key backup files.
make_healthy_env() {
  cp "$REPO_ROOT/tests/fixtures/node.toml" "$MONAD_HOME/monad-bft/config/node.toml"
  echo "KEYSTORE_PASSWORD='testpass'" > "$MONAD_HOME/.env"

  # services are running, as on a real synced full node (the monad-status
  # mock reports in-sync only while this marker exists)
  touch "$MOCK_LOG.active"

  # the full node's own (pre-existing) keys
  monad-keystore import --ikm "9999999999999999999999999999999999999999999999999999999999999999" \
    --keystore-path "$MONAD_HOME/monad-bft/config/id-secp" --password "testpass"
  monad-keystore import --ikm "8888888888888888888888888888888888888888888888888888888888888888" \
    --keystore-path "$MONAD_HOME/monad-bft/config/id-bls" --password "testpass"

  # the validator's key backup files (official format)
  {
    echo "Secp public key: 0xSECPvalidator"
    echo "Keystore secret: $SECP_IKM"
  } > "$BACKUP_ROOT/secp-backup"
  {
    echo "BLS public key: 0xBLSvalidator"
    echo "Keystore secret: $BLS_IKM"
  } > "$BACKUP_ROOT/bls-backup"
}

@test "--version prints the version" {
  run bash "$SCRIPT" --version
  [ "$status" -eq 0 ]
  [[ "$output" == monad-failover\ v* ]]
}

@test "--help exits 0 and shows usage" {
  run bash "$SCRIPT" --help
  [ "$status" -eq 0 ]
  [[ "$output" == *"Usage:"* ]]
  [[ "$output" == *"--dry-run"* ]]
}

@test "unknown flag exits 1" {
  run bash "$SCRIPT" --bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"Unknown argument"* ]]
}

@test "dry-run fails on an empty environment" {
  run bash "$SCRIPT" --dry-run
  [ "$status" -eq 1 ]
  [[ "$output" == *"Preflight failed"* ]]
}

@test "dry-run passes on a healthy environment" {
  make_healthy_env
  run bash "$SCRIPT" --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"valid IKM format"* ]]
  [[ "$output" == *"Preflight passed"* ]]
}

@test "dry-run warns when a backup file has no valid IKM" {
  make_healthy_env
  echo "garbage" > "$BACKUP_ROOT/secp-backup"
  run bash "$SCRIPT" --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"contains no valid IKM"* ]]
}

@test "live run refuses when the node is not in sync" {
  make_healthy_env
  MOCK_STATUS="syncing" run bash "$SCRIPT" </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"Must be fully synced"* ]]
}

@test "live run dies on a corrupt key backup file" {
  make_healthy_env
  echo "Keystore secret: nothex" > "$BACKUP_ROOT/secp-backup"
  run bash "$SCRIPT" --backup-dir "$BACKUP_ROOT" <<'EOF'
y
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"Could not extract a valid SECP IKM"* ]]
}

@test "full promotion flow succeeds end-to-end" {
  make_healthy_env

  # answers: host ok / key source: backup files / dir: default /
  # keys match / beneficiary / node_name / final seq / STOPPED confirm / cutover
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
2
STOPPED
y
EOF

  [ "$status" -eq 0 ]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]

  # the uptime API was queried and reported the validator active
  [[ "$output" == *"MockVal"* ]]
  [[ "$output" == *"Uptime (24h): 100%"* ]]

  local cfg="$MONAD_HOME/monad-bft/config"

  # validator keys swapped in, staging files gone
  grep -q "ikm=$SECP_IKM" "$cfg/id-secp"
  grep -q "ikm=$BLS_IKM" "$cfg/id-bls"
  [ ! -f "$cfg/id-secp.new" ]
  [ ! -f "$cfg/id-bls.new" ]

  # node.toml fully patched; seq flows verbatim: user enters the FINAL value 2
  # → script passes 2 → signer emits 2 → node.toml has 2 (no math anywhere)
  grep -q '^beneficiary = "0xBEEF00000000000000000000000000000000BEEF"' "$cfg/node.toml"
  grep -q '^node_name = "validator-one"' "$cfg/node.toml"
  grep -q '^self_record_seq_num = 2' "$cfg/node.toml"
  grep -q '^self_address = "203.0.113.7:8000"' "$cfg/node.toml"
  grep -q '^enable_publisher = true' "$cfg/node.toml"
  grep -q '^enable_client = true' "$cfg/node.toml"
  grep -q '^expand_to_group = true' "$cfg/node.toml"

  # the full node's own identity was backed up before the swap
  local bdir
  bdir="$(find "$BACKUP_ROOT" -maxdepth 1 -type d -name 'failover-*' | head -1)"
  [ -n "$bdir" ]
  grep -q "ikm=9999" "$bdir/id-secp"
  [ -f "$bdir/node.toml" ]
  # the keystore password must NOT be colocated with the encrypted keys
  [ ! -f "$bdir/keystore-password-backup" ]
  [ ! -f "$bdir/.env" ]

  # fresh key backups re-exported from the live keys; old ones preserved
  grep -q "Keystore secret: $SECP_IKM" "$BACKUP_ROOT/secp-backup"
  grep -q "Keystore secret: $BLS_IKM" "$BACKUP_ROOT/bls-backup"
  ls "$BACKUP_ROOT"/secp-backup.*.bak >/dev/null

  # services were stopped before the swap and started after
  grep -q "systemctl stop monad-bft" "$MOCK_LOG"
  grep -q "systemctl start monad-bft" "$MOCK_LOG"

  # resume state cleared after success
  [ ! -f "$MF_STATE_DIR/state" ]

  # the run was recorded to a log file
  local logf
  logf="$(find "$LOG_DIR" -name 'failover-*.log' | head -1)"
  [ -n "$logf" ]
  grep -q "VALIDATOR PROMOTION COMPLETE" "$logf"
}

@test "signer emitting a higher seq warns but completes (version drift)" {
  make_healthy_env
  export MOCK_SEQ_OFFSET=1
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
2
STOPPED
y
EOF
  [ "$status" -eq 0 ]
  [[ "$output" == *"this monad version increments it"* ]]
  grep -q '^self_record_seq_num = 3' "$MONAD_HOME/monad-bft/config/node.toml"
}

@test "signer emitting a LOWER seq aborts before cutover (stale-seq guard)" {
  make_healthy_env
  export MOCK_SEQ_OFFSET=-1
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
2
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"lower than the requested"* ]]
  # nothing swapped: the full node's own key is untouched
  grep -q "ikm=9999" "$MONAD_HOME/monad-bft/config/id-secp"
}

@test "--public-ip overrides detection end-to-end" {
  make_healthy_env
  run bash "$SCRIPT" --public-ip 198.51.100.9 <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
2
STOPPED
y
EOF
  [ "$status" -eq 0 ]
  grep -q '^self_address = "198.51.100.9:8000"' "$MONAD_HOME/monad-bft/config/node.toml"
}

@test "invalid --public-ip is rejected" {
  run bash "$SCRIPT" --public-ip 999.1.2.3
  [ "$status" -eq 1 ]
  [[ "$output" == *"valid IPv4"* ]]
}

@test "seq_num of zero or garbage is rejected" {
  make_healthy_env
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
0
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"Must be a positive number"* ]]
}

@test "abort at STOPPED leaves the live node fully untouched" {
  make_healthy_env
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
2
yes
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"Not confirmed"* ]]

  local cfg="$MONAD_HOME/monad-bft/config"
  # live keys untouched
  grep -q "ikm=9999" "$cfg/id-secp"
  grep -q "ikm=8888" "$cfg/id-bls"
  # live node.toml untouched: original name kept, validator values absent
  grep -q '^node_name = "fullnode-one"' "$cfg/node.toml"
  ! grep -q "0xBEEF00000000000000000000000000000000BEEF" "$cfg/node.toml"
  ! grep -q '^enable_publisher = true' "$cfg/node.toml"
  # all changes live only in the staging copy
  grep -q '^node_name = "validator-one"' "$cfg/node.toml.new"
}

@test "service crashing right after start is caught, then resume completes" {
  make_healthy_env
  export MOCK_CRASH_AFTER_START=1
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
2
STOPPED
y
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"not active after cutover"* ]]
  # cutover itself is done and recorded; state kept for --resume
  grep -q "last_step=7" "$MF_STATE_DIR/state"

  unset MOCK_CRASH_AFTER_START
  touch "$MOCK_LOG.active"   # operator fixed it; services are up again
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 0 ]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]
}

@test "malformed beneficiary is rejected before any cutover" {
  make_healthy_env
  run bash "$SCRIPT" <<EOF
y
1

y
not-an-address
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"beneficiary must be"* ]]
  grep -q "ikm=9999" "$MONAD_HOME/monad-bft/config/id-secp"
}

@test "manual IKM entry promotes successfully" {
  make_healthy_env
  rm -f "$BACKUP_ROOT/secp-backup" "$BACKUP_ROOT/bls-backup"
  run bash "$SCRIPT" <<EOF
y
2
$SECP_IKM
$BLS_IKM
y
0xBEEF00000000000000000000000000000000BEEF
validator-one
1
STOPPED
y
EOF
  [ "$status" -eq 0 ]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]
  grep -q "ikm=$SECP_IKM" "$MONAD_HOME/monad-bft/config/id-secp"
}

@test "resume completes after services fail to start during cutover" {
  make_healthy_env
  export MOCK_FAIL_START="$BATS_TEST_TMPDIR/failstart"
  touch "$MOCK_FAIL_START"

  # First run: dies when the services fail to start, AFTER the key swap.
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
1
STOPPED
y
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"failed to start"* ]]

  local cfg="$MONAD_HOME/monad-bft/config"
  # keys AND config already swapped; staging consumed; state advanced
  grep -q "ikm=$SECP_IKM" "$cfg/id-secp"
  grep -q '^node_name = "validator-one"' "$cfg/node.toml"
  [ ! -f "$cfg/id-secp.new" ]
  [ ! -f "$cfg/node.toml.new" ]
  grep -q "last_step=7" "$MF_STATE_DIR/state"

  # Resuming while services are still down must be refused by the health gate.
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"not active after cutover"* ]]

  # Operator starts the services (per the printed instructions), then resumes.
  touch "$MOCK_LOG.active"
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 0 ]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]
  [ ! -f "$MF_STATE_DIR/state" ]
}

@test "cutover refuses when a staging key is missing (no service stop)" {
  make_healthy_env
  # Simulate a prior run that reached the cutover gate with state at 6 but
  # only one staging file present.
  mkdir -p "$MF_STATE_DIR"
  cat > "$MF_STATE_DIR/state" <<EOF
last_step=6
network=testnet
new_seq=2
secp_pub=0xSECPtest
bls_pub=0xBLStest
ip=203.0.113.7
self_address=203.0.113.7:8000
self_sig=abcd
self_seq=2
beneficiary=0xBEEF00000000000000000000000000000000BEEF
backup_dir=$BACKUP_ROOT/failover-x
EOF
  : > "$MONAD_HOME/monad-bft/config/id-secp.new"   # only one staging key

  run bash "$SCRIPT" --resume <<EOF
STOPPED
y
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"Staging files are missing"* ]]
  # services must NOT have been stopped
  ! grep -q "systemctl stop monad-bft" "$MOCK_LOG"
}

@test "uptime API failure never blocks the promotion" {
  make_healthy_env
  export MOCK_API_FAIL=1
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
1
STOPPED
y
EOF
  [ "$status" -eq 0 ]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]
  [[ "$output" == *"not visible in the uptime API yet"* ]]
}

@test "partial cutover (BLS rename fails) is resumable to completion" {
  make_healthy_env
  export MOCK_FAIL_MV_DEST="$MONAD_HOME/monad-bft/config/id-bls"
  export MOCK_FAIL_MV_FLAG="$BATS_TEST_TMPDIR/failmv"
  touch "$MOCK_FAIL_MV_FLAG"

  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
2
STOPPED
y
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"SECP key was placed but the BLS key was not"* ]]
  [[ "$output" == *"--resume"* ]]

  local cfg="$MONAD_HOME/monad-bft/config"
  # secp already swapped, bls still the old full-node key, its staging intact
  grep -q "ikm=$SECP_IKM" "$cfg/id-secp"
  grep -q "ikm=8888" "$cfg/id-bls"
  [ -f "$cfg/id-bls.new" ]
  grep -q "last_step=6" "$MF_STATE_DIR/state"

  # --resume finishes the interrupted cutover: the placed secp is recognized
  # by its recorded checksum, the remaining files are moved, services start.
  run bash "$SCRIPT" --resume <<EOF
STOPPED
y
EOF
  [ "$status" -eq 0 ]
  [[ "$output" == *"already in place from a previous cutover attempt"* ]]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]
  grep -q "ikm=$BLS_IKM" "$cfg/id-bls"
  grep -q '^node_name = "validator-one"' "$cfg/node.toml"
  [ ! -f "$MF_STATE_DIR/state" ]
}

@test "crash after full swap but before start: resume recognizes placed files" {
  make_healthy_env
  local cfg="$MONAD_HOME/monad-bft/config"

  # Simulate: cutover swapped all three files, then the machine died before
  # the swap could be recorded — worst case, last_step still 6, staging gone.
  monad-keystore import --ikm "$SECP_IKM" --keystore-path "$cfg/id-secp" --password "testpass"
  monad-keystore import --ikm "$BLS_IKM" --keystore-path "$cfg/id-bls" --password "testpass"
  rm -f "$MOCK_LOG.active"   # services were stopped for the cutover

  mkdir -p "$MF_STATE_DIR"
  cat > "$MF_STATE_DIR/state" <<EOF
last_step=6
cutover_started=1
network=testnet
new_seq=2
secp_pub=0xSECP${SECP_IKM:0:40}
bls_pub=0xBLS${BLS_IKM:0:40}
ip=203.0.113.7
self_address=203.0.113.7:8000
self_sig=abcd
self_seq=2
beneficiary=0xBEEF00000000000000000000000000000000BEEF
backup_dir=$BACKUP_ROOT/failover-x
staged_secp_sha=$(sha256sum "$cfg/id-secp" | awk '{print $1}')
staged_bls_sha=$(sha256sum "$cfg/id-bls" | awk '{print $1}')
staged_toml_sha=$(sha256sum "$cfg/node.toml" | awk '{print $1}')
EOF

  run bash "$SCRIPT" --resume <<EOF
STOPPED
y
EOF
  [ "$status" -eq 0 ]
  [[ "$output" == *"already in place from a previous cutover attempt"* ]]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]
  grep -q "systemctl start monad-bft" "$MOCK_LOG"
  [ ! -f "$MF_STATE_DIR/state" ]
}

@test "fresh run is refused while an interrupted cutover exists" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  cat > "$MF_STATE_DIR/state" <<EOF
last_step=6
cutover_started=1
EOF
  run bash "$SCRIPT" </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"reached cutover"* ]]
  [[ "$output" == *"--resume"* ]]
  # the state must survive: it is the only map of how far the cutover got
  grep -q "cutover_started=1" "$MF_STATE_DIR/state"
}

@test "failed key backup export keeps resume state; resume retries only the export" {
  make_healthy_env
  export MOCK_FAIL_RECOVER_PATH="$MONAD_HOME/monad-bft/config/id-secp"

  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
2
STOPPED
y
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"Key backup export failed"* ]]
  [[ "$output" == *"--resume"* ]]
  # the promotion itself happened; state kept at 7 so --resume lands in step 8
  grep -q "last_step=7" "$MF_STATE_DIR/state"
  # the old backup was preserved, no truncated replacement left behind
  ls "$BACKUP_ROOT"/secp-backup.*.bak >/dev/null
  [ ! -f "$BACKUP_ROOT/secp-backup" ]

  unset MOCK_FAIL_RECOVER_PATH
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 0 ]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]
  grep -q "Keystore secret: $SECP_IKM" "$BACKUP_ROOT/secp-backup"
  [ ! -f "$MF_STATE_DIR/state" ]
}

@test ".env with CRLF line endings still yields the exact password" {
  make_healthy_env
  printf "KEYSTORE_PASSWORD='testpass'\r\n" > "$MONAD_HOME/.env"
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
2
STOPPED
y
EOF
  [ "$status" -eq 0 ]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]
  # quotes stripped AND no trailing carriage return in the stored password
  grep -q "pw=testpass\$" "$MONAD_HOME/monad-bft/config/id-secp"
}

@test "leftover state file offers resume and exits cleanly when declined" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  echo "last_step=3" > "$MF_STATE_DIR/state"
  run bash "$SCRIPT" <<'EOF'
n
EOF
  [ "$status" -eq 0 ]
  [[ "$output" == *"--resume"* ]]
  # state must survive the declined prompt
  grep -q "last_step=3" "$MF_STATE_DIR/state"
}

# ── state trust boundary ───────────────────────────────────
# A resume run reads this state back and acts on it while holding root. These
# cover that boundary rather than the promotion flow: on a real node the old
# location was under $MONAD_HOME, writable by the unprivileged monad account.

@test "state: command injection in last_step is refused, not evaluated" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  # $(( )) evaluates an array subscript, and a subscript runs command
  # substitution: unvalidated, this line would execute as root.
  printf 'last_step=PATH[$(touch %s/PWNED)]\n' "$BATS_TEST_TMPDIR" > "$MF_STATE_DIR/state"
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"last_step"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/PWNED" ]
}

@test "state: injected last_step is refused on a non-resume run too" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  printf 'last_step=PATH[$(touch %s/PWNED)]\n' "$BATS_TEST_TMPDIR" > "$MF_STATE_DIR/state"
  run bash "$SCRIPT" </dev/null
  [ "$status" -eq 1 ]
  [ ! -e "$BATS_TEST_TMPDIR/PWNED" ]
}

@test "state: a symlinked state file is refused and its target is untouched" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  echo "important" > "$BATS_TEST_TMPDIR/victim"
  ln -s "$BATS_TEST_TMPDIR/victim" "$MF_STATE_DIR/state"
  run bash "$SCRIPT" </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"symlink"* ]]
  [ "$(cat "$BATS_TEST_TMPDIR/victim")" = "important" ]
}

@test "state: a symlinked state directory is refused" {
  make_healthy_env
  mkdir -p "$BATS_TEST_TMPDIR/var/lib" "$BATS_TEST_TMPDIR/elsewhere"
  ln -s "$BATS_TEST_TMPDIR/elsewhere" "$MF_STATE_DIR"
  run bash "$SCRIPT" </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"symlink"* ]]
}

@test "state: legacy state under MONAD_HOME is refused, never migrated" {
  make_healthy_env
  mkdir -p "$MONAD_HOME/.monad-failover"
  echo "last_step=6" > "$MONAD_HOME/.monad-failover/state"
  run bash "$SCRIPT" </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"older version"* ]]
  # not adopted into the new location...
  [ ! -f "$MF_STATE_DIR/state" ]
  # ...and left in place for the operator to inspect
  grep -q "last_step=6" "$MONAD_HOME/.monad-failover/state"
}

@test "state: MF_STATE_DIR is refused outside the test sandbox" {
  run env -u MF_ALLOW_NONROOT MF_STATE_DIR="$BATS_TEST_TMPDIR/anywhere" \
    bash "$SCRIPT" --version
  [ "$status" -eq 1 ]
  [[ "$output" == *"MF_STATE_DIR"* ]]
}

@test "state: backup_dir pointing outside the backup root is refused" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  cat > "$MF_STATE_DIR/state" <<EOF
last_step=6
backup_dir=$BATS_TEST_TMPDIR/evil
EOF
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"backup_dir"* ]]
}

@test "state: loose permissions on the state dir and file are tightened" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  echo "last_step=3" > "$MF_STATE_DIR/state"
  chmod 777 "$MF_STATE_DIR"
  chmod 666 "$MF_STATE_DIR/state"
  run bash "$SCRIPT" <<'EOF'
n
EOF
  [ "$status" -eq 0 ]
  [ "$(stat -c '%a' "$MF_STATE_DIR")" = "700" ]
  [ "$(stat -c '%a' "$MF_STATE_DIR/state")" = "600" ]
}

@test "state: an out-of-range last_step is refused (no fake completion)" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  echo "last_step=999" > "$MF_STATE_DIR/state"
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"last_step"* ]]
  [[ "$output" != *"PROMOTION COMPLETE"* ]]
}

@test "state: reaching step 6 with an empty signed field is refused" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  cat > "$MF_STATE_DIR/state" <<EOF
last_step=6
network=testnet
secp_pub=0xSECPvalidator
bls_pub=0xBLSvalidator
new_seq=8
ip=
self_address=
self_sig=
self_seq=
EOF
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"incomplete"* || "$output" == *"empty"* ]]
}

@test "state: a bogus network value is refused" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  printf 'last_step=2\nnetwork=evil$(id)\n' > "$MF_STATE_DIR/state"
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"network"* ]]
}

@test "state: a malformed beneficiary in state is refused" {
  make_healthy_env
  mkdir -p "$MF_STATE_DIR"
  printf 'last_step=5\nnetwork=testnet\nsecp_pub=0xSECPvalidator\nbls_pub=0xBLSvalidator\nnew_seq=8\nbeneficiary=0xnothex\n' > "$MF_STATE_DIR/state"
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"beneficiary"* ]]
}
