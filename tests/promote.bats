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
  export MOCK_LOG="$BATS_TEST_TMPDIR/mock.log"
  export PATH="$REPO_ROOT/tests/mocks:$PATH"
  mkdir -p "$MONAD_HOME/monad-bft/config" "$BACKUP_ROOT"
  touch "$MOCK_LOG"
}

# Build a healthy full-node environment: config, .env, live full-node keys,
# and valid validator key backup files.
make_healthy_env() {
  cp "$REPO_ROOT/tests/fixtures/node.toml" "$MONAD_HOME/monad-bft/config/node.toml"
  echo "KEYSTORE_PASSWORD='testpass'" > "$MONAD_HOME/.env"

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
  # keys match / beneficiary / node_name / last seq / STOPPED confirm / cutover
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

  local cfg="$MONAD_HOME/monad-bft/config"

  # validator keys swapped in, staging files gone
  grep -q "ikm=$SECP_IKM" "$cfg/id-secp"
  grep -q "ikm=$BLS_IKM" "$cfg/id-bls"
  [ ! -f "$cfg/id-secp.new" ]
  [ ! -f "$cfg/id-bls.new" ]

  # node.toml fully patched
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
  [ ! -f "$MONAD_HOME/.monad-failover/state" ]
}

@test "cutover is refused unless the operator types STOPPED" {
  make_healthy_env
  run bash "$SCRIPT" <<EOF
y
1

y
0xBEEF00000000000000000000000000000000BEEF
validator-one
1
yes
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"Not confirmed"* ]]
  # nothing swapped: the full node's own key is untouched
  grep -q "ikm=9999" "$MONAD_HOME/monad-bft/config/id-secp"
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
  # keys already swapped; staging consumed; state advanced so resume won't re-swap
  grep -q "ikm=$SECP_IKM" "$cfg/id-secp"
  [ ! -f "$cfg/id-secp.new" ]
  grep -q "last_step=7" "$MONAD_HOME/.monad-failover/state"

  # Resume: services start fine now, bookkeeping finishes, state cleared.
  run bash "$SCRIPT" --resume </dev/null
  [ "$status" -eq 0 ]
  [[ "$output" == *"VALIDATOR PROMOTION COMPLETE"* ]]
  [ ! -f "$MONAD_HOME/.monad-failover/state" ]
}

@test "cutover refuses when a staging key is missing (no service stop)" {
  make_healthy_env
  # Simulate a prior run that reached the cutover gate with state at 6 but
  # only one staging file present.
  mkdir -p "$MONAD_HOME/.monad-failover"
  cat > "$MONAD_HOME/.monad-failover/state" <<EOF
last_step=6
network=testnet
new_seq=2
secp_pub=0xSECPtest
bls_pub=0xBLStest
ip=203.0.113.7
self_address=203.0.113.7:8000
self_sig=abcd
beneficiary=0xBEEF00000000000000000000000000000000BEEF
backup_dir=$BACKUP_ROOT/failover-x
EOF
  : > "$MONAD_HOME/monad-bft/config/id-secp.new"   # only one staging key

  run bash "$SCRIPT" --resume <<EOF
STOPPED
y
EOF
  [ "$status" -eq 1 ]
  [[ "$output" == *"Staging keys are missing"* ]]
  # services must NOT have been stopped
  ! grep -q "systemctl stop monad-bft" "$MOCK_LOG"
}

@test "leftover state file offers resume and exits cleanly when declined" {
  make_healthy_env
  mkdir -p "$MONAD_HOME/.monad-failover"
  echo "last_step=3" > "$MONAD_HOME/.monad-failover/state"
  run bash "$SCRIPT" <<'EOF'
n
EOF
  [ "$status" -eq 0 ]
  [[ "$output" == *"--resume"* ]]
  # state must survive the declined prompt
  grep -q "last_step=3" "$MONAD_HOME/.monad-failover/state"
}
