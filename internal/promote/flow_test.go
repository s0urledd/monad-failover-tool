package promote

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/s0urledd/monad-failover-tool/internal/paths"
	"github.com/s0urledd/monad-failover-tool/internal/place"
	"github.com/s0urledd/monad-failover-tool/internal/state"
	"github.com/s0urledd/monad-failover-tool/internal/testutil"
	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

// The flow tests drive the real migration in-process against the shell
// suite's mock Monad binaries (tests/mocks on PATH) and one local HTTP
// server standing in for every endpoint. No network, no systemd, no real
// keys: everything happens in a temporary directory.

const (
	beneficiary = "0xBEEF00000000000000000000000000000000BEEF"
	nodeName    = "validator-one"
	publicIP    = "203.0.113.7"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

type harness struct {
	t    *testing.T
	root string
	p    paths.Paths
	d    state.Dir
	ep   *testutil.Endpoints
	log  string // MOCK_LOG
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{t: t, root: root}
	home := filepath.Join(root, "home", "monad")
	h.p = paths.Paths{
		MonadHome:        home,
		ConfigDir:        filepath.Join(home, "monad-bft", "config"),
		NodeToml:         filepath.Join(home, "monad-bft", "config", "node.toml"),
		EnvFile:          filepath.Join(home, ".env"),
		SecpKey:          filepath.Join(home, "monad-bft", "config", "id-secp"),
		BlsKey:           filepath.Join(home, "monad-bft", "config", "id-bls"),
		PubkeyList:       filepath.Join(home, "pubkey-secp-bls"),
		BackupRoot:       filepath.Join(root, "opt", "monad", "backup"),
		LogDir:           filepath.Join(root, "opt", "monad", "failover-logs"),
		Sandbox:          true,
		EUID:             os.Geteuid(),
		HealthWait:       0,
		SyncWait:         0,
		FoundationMaxAge: 86400 * time.Second,
	}
	h.d = state.Resolve(true, filepath.Join(root, "var", "lib", "monad-failover"))
	os.MkdirAll(h.p.ConfigDir, 0o755)
	os.MkdirAll(h.p.BackupRoot, 0o700)

	h.log = filepath.Join(root, "mock.log")
	os.WriteFile(h.log, nil, 0o644)
	t.Setenv("MOCK_LOG", h.log)
	t.Setenv("PATH", filepath.Join(repoRoot(t), "tests", "mocks")+":"+os.Getenv("PATH"))
	for _, k := range []string{"MOCK_PREMASKED", "MOCK_MASK_FAIL", "MOCK_FAIL_START", "MOCK_CRASH_AFTER_START",
		"MOCK_STATUS", "MOCK_STATUS_AFTER", "MOCK_SEQ_OFFSET", "MOCK_UDP_PORT", "MOCK_SIGNER_OMIT", "MOCK_FAIL_RECOVER_PATH"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	h.ep = testutil.NewEndpoints()
	t.Cleanup(h.ep.Close)
	h.p.FoundationBase = h.ep.FoundationBase()
	h.p.IPURL = h.ep.IPURL()
	h.p.UptimeMainnet, h.p.UptimeTestnet = h.ep.UptimeBase(), h.ep.UptimeBase()
	return h
}

// healthyEnv builds a synced full node: config, .env, live full-node keys,
// running services, and valid validator key backup files.
func (h *harness) healthyEnv() {
	h.t.Helper()
	fixture, err := os.ReadFile(filepath.Join(repoRoot(h.t), "tests", "fixtures", "node.toml"))
	if err != nil {
		h.t.Fatal(err)
	}
	os.WriteFile(h.p.NodeToml, fixture, 0o644)
	os.WriteFile(h.p.EnvFile, []byte("KEYSTORE_PASSWORD='testpass'\n"), 0o600)
	os.WriteFile(h.log+".active", nil, 0o644)
	h.mock("monad-keystore", "import", "--ikm", strings.Repeat("9", 64), "--keystore-path", h.p.SecpKey, "--password", "testpass")
	h.mock("monad-keystore", "import", "--ikm", strings.Repeat("8", 64), "--keystore-path", h.p.BlsKey, "--password", "testpass")
	os.WriteFile(filepath.Join(h.p.BackupRoot, "secp-backup"),
		[]byte("Secp public key: 0xSECPvalidator\nKeystore secret: "+testutil.SecpIKM+"\n"), 0o600)
	os.WriteFile(filepath.Join(h.p.BackupRoot, "bls-backup"),
		[]byte("BLS public key: 0xBLSvalidator\nKeystore secret: "+testutil.BlsIKM+"\n"), 0o600)
}

func (h *harness) mock(name string, args ...string) {
	h.t.Helper()
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// run executes a live run with the given stdin script and returns the exit
// code and the ANSI-stripped combined output.
func (h *harness) run(stdin string, opt Options) (int, string) {
	h.t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		h.t.Fatal(err)
	}
	w.WriteString(stdin)
	w.Close()
	defer r.Close()
	var buf bytes.Buffer
	c := ui.New(&buf, &buf, r)
	opt.Version, opt.Argv0 = "test", "monad-failover"
	run := New(c, h.p, h.d, opt)
	run.sleep = func(time.Duration) {}

	code := 0
	proceed, lock, err := run.Prepare()
	if lock != nil {
		defer lock.Release()
	}
	switch {
	case err != nil:
		c.Report(err)
		code = 1
	case !proceed:
	default:
		if err := run.Promote(); err != nil {
			c.Report(err)
			code = 1
		}
	}
	return code, ansi.ReplaceAllString(buf.String(), "")
}

func (h *harness) dryRun(keyDir string) (int, string) {
	h.t.Helper()
	var buf bytes.Buffer
	c := ui.New(&buf, &buf, nil)
	code := DryRun(c, h.p, keyDir, "test")
	return code, ansi.ReplaceAllString(buf.String(), "")
}

// normalStdin answers every prompt of a healthy run through completion.
func normalStdin(ben, name, seq string) string {
	return "y\ny\n" + ben + "\n" + name + "\n" + seq + "\nSTOPPED\ny\n"
}

func (h *harness) normalOpts() Options {
	return Options{KeySourceDir: h.p.BackupRoot, PublicIP: publicIP}
}

func (h *harness) normalRun() (int, string) {
	return h.run(normalStdin(beneficiary, nodeName, "8"), h.normalOpts())
}

func (h *harness) mockLog() string {
	b, _ := os.ReadFile(h.log)
	return string(b)
}

func (h *harness) stateValue(key string) string { return h.d.Store().Get(key) }

func (h *harness) stateExists() bool { return h.d.Store().Exists() }

func (h *harness) read(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func (h *harness) liveSHAs() [3]string {
	return [3]string{place.FileSHA(h.p.SecpKey), place.FileSHA(h.p.BlsKey), place.FileSHA(h.p.NodeToml)}
}

func (h *harness) runLog() string {
	entries, _ := os.ReadDir(h.p.LogDir)
	var all string
	for _, e := range entries {
		all += h.read(filepath.Join(h.p.LogDir, e.Name()))
	}
	return all
}

func expect(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func reject(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if strings.Contains(out, w) {
			t.Errorf("output must not contain %q:\n%s", w, out)
		}
	}
}

// tomlIn reports whether `key = value` appears inside [table] ("" = root).
func tomlIn(content, table, key, value string) bool {
	cur := ""
	for _, l := range strings.Split(content, "\n") {
		if strings.HasPrefix(l, "[") {
			cur = strings.Trim(strings.TrimSpace(l), "[]")
			continue
		}
		if cur == table && l == key+" = "+value {
			return true
		}
	}
	return false
}

func (h *harness) assertServicesUntouched() {
	h.t.Helper()
	if l := h.mockLog(); regexp.MustCompile(`systemctl (stop|start|mask|unmask)`).MatchString(l) {
		h.t.Errorf("services were touched:\n%s", l)
	}
}

// ── the reviewer's four properties, then everything the shell suite covers ──

func TestFullPromotionEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	before := h.liveSHAs()
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "VALIDATOR PROMOTION COMPLETE")
	reject(t, out, "Resuming from step")
	expect(t, out,
		"Node: in-sync (block difference: 0)",
		"Network: testnet",
		"Public IP: "+publicIP,
		"IKM secrets extracted from backup files",
		"SECP: "+testutil.MockSecp,
		"Keys verified",
		"Beneficiary: "+beneficiary,
		"node_name: "+nodeName,
		"Last published sequence for this key: 7",
		"seq_num for this migration: 8",
		"Name record signed (seq 8)",
		"Old validator confirmed stopped or offline",
		"Services masked for the swap",
		"SECP key placed", "BLS key placed", "node.toml placed",
		"Live identity matches what was placed",
		"All required services active",
		"MockVal is active on testnet (uptime API)",
		"Key backups exported",
		"Block public access to RPC and metrics ports (8080, 8081, 9143, etc.).",
	)
	if h.liveSHAs() == before {
		t.Error("live identity was not replaced")
	}
	toml := h.read(h.p.NodeToml)
	for _, c := range []struct{ table, key, value string }{
		{"", "beneficiary", `"` + beneficiary + `"`},
		{"", "node_name", `"` + nodeName + `"`},
		{"peer_discovery", "self_address", `"` + publicIP + `:8000"`},
		{"peer_discovery", "self_auth_port", "8001"},
		{"peer_discovery", "self_record_seq_num", "8"},
		{"fullnode_raptorcast", "enable_publisher", "true"},
		{"fullnode_raptorcast", "enable_client", "true"},
		{"statesync", "expand_to_group", "true"},
	} {
		if !tomlIn(toml, c.table, c.key, c.value) {
			t.Errorf("node.toml lacks %s.%s = %s:\n%s", c.table, c.key, c.value, toml)
		}
	}
	if !strings.Contains(h.read(h.p.SecpKey), "ikm="+testutil.SecpIKM) || !strings.Contains(h.read(h.p.BlsKey), "ikm="+testutil.BlsIKM) {
		t.Error("validator keys not in place")
	}
	if !strings.Contains(h.read(filepath.Join(h.p.BackupRoot, "secp-backup")), "Keystore secret: "+testutil.SecpIKM) {
		t.Error("fresh secp backup not exported")
	}
	if h.stateExists() {
		t.Error("state not cleared after completion")
	}
	for _, f := range []string{h.d.SecpNew, h.d.BlsNew, h.d.TomlNew} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("staging file left: %s", f)
		}
	}
	seq := regexp.MustCompile(`systemctl (mask|stop|unmask|enable|start) `).FindAllString(h.mockLog(), -1)
	if got := strings.Join(seq, ""); got != "systemctl mask systemctl stop systemctl unmask systemctl unmask systemctl unmask systemctl enable systemctl start " {
		t.Errorf("service sequence: %q", got)
	}
	log := h.runLog()
	if strings.Contains(log, testutil.SecpIKM) || strings.Contains(log, testutil.BlsIKM) || strings.Contains(log, "testpass") {
		t.Error("a secret reached the run log")
	}
	entries, _ := os.ReadDir(h.p.LogDir)
	fi, _ := entries[0].Info()
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("run log mode %v", fi.Mode())
	}
	// the previous identity was preserved for manual restore
	dirs, _ := filepath.Glob(filepath.Join(h.p.BackupRoot, "failover-*"))
	if len(dirs) != 1 || !strings.Contains(h.read(filepath.Join(dirs[0], "id-secp")), "ikm="+strings.Repeat("9", 64)) {
		t.Errorf("identity backup: %v", dirs)
	}
}

func TestAbortAtStoppedLeavesLiveNodeUntouched(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	before := h.liveSHAs()
	code, out := h.run("y\ny\n"+beneficiary+"\n"+nodeName+"\n8\nnope\n", h.normalOpts())
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Not confirmed — aborting before cutover.")
	if h.liveSHAs() != before {
		t.Error("live files changed before cutover")
	}
	h.assertServicesUntouched()
	if h.stateValue("last_step") != "6" {
		t.Errorf("last_step = %q", h.stateValue("last_step"))
	}
}

// 1. A key or config changed after the operator confirmed it stops the cutover.
func TestStagedKeyChangedAfterConfirmationIsRefused(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	before := h.liveSHAs()
	if code, out := h.run("y\ny\n"+beneficiary+"\n"+nodeName+"\n8\nnope\n", h.normalOpts()); code != 1 {
		t.Fatalf("setup exit %d:\n%s", code, out)
	}
	os.WriteFile(h.d.SecpNew, []byte("MOCK-KEYSTORE ikm="+strings.Repeat("f", 64)+" pw=testpass\n"), 0o600)
	code, out := h.run("STOPPED\ny\n", Options{Resume: true, PublicIP: publicIP})
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "the SECP key changed after it was prepared and confirmed.", "Nothing has been changed on the live node.")
	h.assertServicesUntouched()
	if h.liveSHAs() != before {
		t.Error("live files changed")
	}
	if h.stateValue("cutover_started") == "1" {
		t.Error("cutover marked started")
	}
}

func TestStagedConfigChangedAfterSigningIsRefused(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	if code, out := h.run("y\ny\n"+beneficiary+"\n"+nodeName+"\n8\nnope\n", h.normalOpts()); code != 1 {
		t.Fatalf("setup exit %d:\n%s", code, out)
	}
	toml := h.read(h.d.TomlNew)
	os.WriteFile(h.d.TomlNew, []byte(strings.Replace(toml, beneficiary, "0x1111111111111111111111111111111111111111", 1)), 0o600)
	code, out := h.run("STOPPED\ny\n", Options{Resume: true, PublicIP: publicIP})
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "node.toml changed after it was prepared and confirmed.")
	h.assertServicesUntouched()
}

func TestMalformedBeneficiaryRejectedBeforeAnyChange(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	before := h.liveSHAs()
	code, out := h.run("y\ny\n0xnotanaddress\n", h.normalOpts())
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "beneficiary must be a 0x-prefixed 40-hex-character address")
	h.assertServicesUntouched()
	if h.liveSHAs() != before {
		t.Error("live files changed")
	}
}

func TestSignerLowerSeqAbortsBeforeCutover(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	t.Setenv("MOCK_SEQ_OFFSET", "-1")
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Signer emitted seq 7, lower than the requested 8.")
	h.assertServicesUntouched()
}

func TestSignerHigherSeqWarnsButCompletes(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	t.Setenv("MOCK_SEQ_OFFSET", "1")
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Signer emitted seq 9 (requested 8)", "Name record signed (seq 9)", "VALIDATOR PROMOTION COMPLETE")
	if !tomlIn(h.read(h.p.NodeToml), "peer_discovery", "self_record_seq_num", "9") {
		t.Error("node.toml does not carry the emitted seq")
	}
}

func TestSignerDifferingPortsRefusedBeforeAnyWrite(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	t.Setenv("MOCK_UDP_PORT", "8002")
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Signer emitted different TCP and UDP ports (8000 / 8002).")
	if strings.Contains(h.read(h.d.TomlNew), "self_name_record_sig = \"ab") {
		t.Error("signature written despite refusal")
	}
	h.assertServicesUntouched()
}

// 2. A cutover interrupted between file placements resumes to completion.
func TestPartialCutoverIsResumable(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	fired := false
	old := place.Rename
	place.Rename = func(a, b string) error {
		if b == h.p.BlsKey && !fired {
			fired = true
			return errors.New("simulated rename failure")
		}
		return os.Rename(a, b)
	}
	defer func() { place.Rename = old }()

	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "CRITICAL: the SECP key was placed but the BLS key was not.", "Do NOT start the services.")
	if !strings.Contains(h.read(h.p.SecpKey), "ikm="+testutil.SecpIKM) {
		t.Error("SECP key should have been placed")
	}
	if strings.Contains(h.read(h.p.BlsKey), "ikm="+testutil.BlsIKM) {
		t.Error("BLS key must not have been placed")
	}
	if h.stateValue("cutover_started") != "1" || h.stateValue("swap_done") == "1" {
		t.Errorf("state: cutover_started=%q swap_done=%q", h.stateValue("cutover_started"), h.stateValue("swap_done"))
	}
	if strings.Contains(h.mockLog(), "systemctl start") {
		t.Error("services started with a mismatched identity")
	}
	// units stay masked across the interruption
	if out, _ := exec.Command("systemctl", "is-enabled", "monad-bft").Output(); strings.TrimSpace(string(out)) != "masked" {
		t.Errorf("monad-bft not masked while half-swapped: %q", out)
	}

	// A resume into an unfinished swap asks for the two cutover confirmations
	// again before it touches anything.
	code, out = h.run("STOPPED\ny\n", Options{Resume: true, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("resume exit %d:\n%s", code, out)
	}
	expect(t, out, "Resuming from step 7", "SECP key already in place from a previous cutover attempt", "BLS key placed", "node.toml placed", "VALIDATOR PROMOTION COMPLETE")
	if !strings.Contains(h.read(h.p.BlsKey), "ikm="+testutil.BlsIKM) {
		t.Error("BLS key not placed on resume")
	}
	// services come up exactly once, on the resume that completed the swap
	if strings.Count(h.mockLog(), "systemctl start") != 1 {
		t.Errorf("service starts:\n%s", h.mockLog())
	}
	if h.stateExists() {
		t.Error("state not cleared")
	}
}

func TestFreshRunRefusedWhileCutoverInterrupted(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	old := place.Rename
	place.Rename = func(a, b string) error {
		if b == h.p.BlsKey {
			return errors.New("simulated")
		}
		return os.Rename(a, b)
	}
	if code, _ := h.normalRun(); code != 1 {
		t.Fatal("setup did not interrupt")
	}
	place.Rename = old
	code, out := h.run("y\n", h.normalOpts())
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "A previous run reached cutover — starting fresh is not safe now.", "monad-failover --resume")
	if h.stateValue("cutover_started") != "1" {
		t.Error("state was cleared")
	}
}

func TestCrashAfterSwapBeforeStartResumesWithoutRepeatingSwap(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	flag := filepath.Join(h.root, "fail-start")
	os.WriteFile(flag, nil, 0o644)
	t.Setenv("MOCK_FAIL_START", flag)
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "The validator keys are in place, but the services failed to start.", "do NOT re-run the cutover")
	if h.stateValue("swap_done") != "1" || h.stateValue("last_step") != "6" {
		t.Errorf("state swap_done=%q last_step=%q", h.stateValue("swap_done"), h.stateValue("last_step"))
	}
	placed := h.liveSHAs()

	code, out = h.run("", Options{Resume: true, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("resume exit %d:\n%s", code, out)
	}
	expect(t, out, "Files were already swapped by an earlier attempt; bringing services up", "Live identity matches what was placed", "VALIDATOR PROMOTION COMPLETE")
	if h.liveSHAs() != placed {
		t.Error("files re-placed on resume")
	}
	if strings.Count(h.mockLog(), "systemctl stop") != 1 || strings.Count(h.mockLog(), "systemctl mask") != 1 {
		t.Error("swap repeated on resume")
	}
}

func TestLiveFileChangedAfterSwapStopsServicesComingUp(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	flag := filepath.Join(h.root, "fail-start")
	os.WriteFile(flag, nil, 0o644)
	t.Setenv("MOCK_FAIL_START", flag)
	if code, _ := h.normalRun(); code != 1 {
		t.Fatal("setup did not stop after the swap")
	}
	os.WriteFile(h.p.SecpKey, []byte("MOCK-KEYSTORE ikm="+strings.Repeat("e", 64)+" pw=testpass\n"), 0o600)
	starts := strings.Count(h.mockLog(), "systemctl start")
	code, out := h.run("", Options{Resume: true, PublicIP: publicIP})
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "the SECP key no longer matches what the cutover placed.", "Refusing to start the services")
	if strings.Count(h.mockLog(), "systemctl start") != starts {
		t.Error("services started with a changed identity")
	}
}

// 3. A service or API failure after cutover never becomes a false success.
func TestServiceCrashingAfterStartIsCaughtThenResumeCompletes(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	t.Setenv("MOCK_CRASH_AFTER_START", "1")
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "monad-bft is not active after cutover.", "journalctl -xeu monad-bft", "monad-failover --resume")
	reject(t, out, "VALIDATOR PROMOTION COMPLETE")
	if h.stateValue("last_step") != "7" {
		t.Errorf("last_step = %q", h.stateValue("last_step"))
	}
	os.Unsetenv("MOCK_CRASH_AFTER_START")
	h.mock("systemctl", "start", "monad-bft", "monad-execution", "monad-rpc")
	code, out = h.run("", Options{Resume: true, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("resume exit %d:\n%s", code, out)
	}
	expect(t, out, "Resuming from step 8", "All required services active", "VALIDATOR PROMOTION COMPLETE")
}

func TestUptimeAPIFailureNeverBlocks(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	h.ep.Uptime = ""
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Validator not visible in the uptime API yet", "Check later: "+h.ep.UptimeBase()+"/"+testutil.MockSecp, "VALIDATOR PROMOTION COMPLETE")
}

func TestUptimeNullLastRoundDoesNotInterruptCompletion(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	h.ep.Uptime = `{"success":true,"uptime":{"validator_name":"MockVal","status":"inactive","uptime_percent":0,"finalized_count":0,"timeout_count":0,"last_round":null}}`
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "MockVal is inactive on testnet (uptime API).", "Uptime (24h): 0% (0 finalized, 0 timeout)", "VALIDATOR PROMOTION COMPLETE")
	reject(t, out, "Last round:")
	if !strings.Contains(h.read(filepath.Join(h.p.BackupRoot, "bls-backup")), "Keystore secret: "+testutil.BlsIKM) {
		t.Error("backup export skipped")
	}
	if h.stateExists() {
		t.Error("state kept")
	}
}

func TestUptimeMissingFieldsDoNotInterruptBackupExport(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	h.ep.Uptime = `{"success":true,"uptime":{}}`
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "validator is unknown on testnet (uptime API).", "Uptime (24h): ?% (? finalized, ? timeout)", "VALIDATOR PROMOTION COMPLETE")
}

func TestNodeNotYetInSyncReportsPendingNotSuccess(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	t.Setenv("MOCK_STATUS_AFTER", "1")
	t.Setenv("MOCK_STATUS", "syncing")
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Node reports syncing after 0s — not in-sync yet.", "CUTOVER COMPLETE — VERIFICATION PENDING", "Key backups exported")
	reject(t, out, "VALIDATOR PROMOTION COMPLETE")
	if h.stateValue("last_step") != "7" {
		t.Errorf("state should stay at step 7, got %q", h.stateValue("last_step"))
	}
	os.Unsetenv("MOCK_STATUS_AFTER")
	os.Unsetenv("MOCK_STATUS")
	os.Remove(h.log + ".statuscount")
	code, out = h.run("", Options{Resume: true, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("resume exit %d:\n%s", code, out)
	}
	expect(t, out, "Node is in-sync", "VALIDATOR PROMOTION COMPLETE")
	if h.stateExists() {
		t.Error("state kept after confirmed sync")
	}
}

func TestFailedBackupExportKeepsStateAndResumeRetriesOnlyExport(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	t.Setenv("MOCK_FAIL_RECOVER_PATH", h.p.BlsKey)
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Could not re-export key backups.", "Key backup export failed after an otherwise successful promotion.")
	reject(t, out, "VALIDATOR PROMOTION COMPLETE")
	if h.stateValue("last_step") != "7" {
		t.Errorf("last_step = %q", h.stateValue("last_step"))
	}
	if _, err := os.Stat(filepath.Join(h.p.BackupRoot, "bls-backup.partial")); err == nil {
		t.Error("partial export left behind")
	}
	baks, _ := filepath.Glob(filepath.Join(h.p.BackupRoot, "*.bak"))
	if len(baks) != 2 {
		t.Errorf("previous backups not preserved: %v", baks)
	}
	os.Unsetenv("MOCK_FAIL_RECOVER_PATH")
	stops := strings.Count(h.mockLog(), "systemctl stop")
	code, out = h.run("", Options{Resume: true, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("resume exit %d:\n%s", code, out)
	}
	expect(t, out, "Key backups exported", "VALIDATOR PROMOTION COMPLETE")
	if strings.Count(h.mockLog(), "systemctl stop") != stops {
		t.Error("resume repeated the cutover")
	}
}

// 4. Units the operator masked beforehand are respected, never silently started.
func TestAllPremaskedUnitsNeverCauseFalseCompletion(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	t.Setenv("MOCK_PREMASKED", "monad-bft monad-execution monad-rpc")
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Every monad unit was already masked before this run; not starting any.",
		"monad-bft was already masked, but is required for validation.",
		"systemctl unmask monad-bft && systemctl start monad-bft")
	reject(t, out, "VALIDATOR PROMOTION COMPLETE")
	if strings.Contains(h.mockLog(), "systemctl start") {
		t.Error("an empty or masked start was issued")
	}
	if h.stateValue("last_step") != "7" {
		t.Errorf("last_step = %q", h.stateValue("last_step"))
	}
	// the operator resolves the intentional masks; resume verifies and exports
	os.Unsetenv("MOCK_PREMASKED")
	h.mock("systemctl", "unmask", "monad-bft", "monad-execution")
	h.mock("systemctl", "start", "monad-bft", "monad-execution")
	code, out = h.run("", Options{Resume: true, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("resume exit %d:\n%s", code, out)
	}
	expect(t, out, "monad-rpc was masked before this run; left unchanged.", "VALIDATOR PROMOTION COMPLETE")
	if o, _ := exec.Command("systemctl", "is-enabled", "monad-rpc").Output(); strings.TrimSpace(string(o)) != "masked" {
		t.Error("monad-rpc was unmasked although the operator had masked it")
	}
}

func TestOperatorMaskedRPCIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	t.Setenv("MOCK_PREMASKED", "monad-rpc")
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "monad-rpc was masked before this run; left unchanged.", "VALIDATOR PROMOTION COMPLETE")
	if strings.Contains(h.mockLog(), "start monad-bft monad-execution monad-rpc") {
		t.Error("monad-rpc was started")
	}
	if !strings.Contains(h.mockLog(), "systemctl start monad-bft monad-execution\n") {
		t.Errorf("expected start of the two unmasked units:\n%s", h.mockLog())
	}
	if o, _ := exec.Command("systemctl", "is-enabled", "monad-rpc").Output(); strings.TrimSpace(string(o)) != "masked" {
		t.Error("monad-rpc unmasked")
	}
}

func TestMaskThatDoesNotTakeEffectStopsBeforeSwap(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	before := h.liveSHAs()
	t.Setenv("MOCK_MASK_FAIL", "1")
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Could not mask monad-bft — refusing to start the swap.", "Nothing has been changed.")
	if strings.Contains(h.mockLog(), "systemctl stop") {
		t.Error("services stopped without a mask")
	}
	if h.liveSHAs() != before {
		t.Error("live files changed")
	}
}

func TestInterruptedMaskIsNotMistakenForTheOperators(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	// A run interrupted after masking but before recording would, without
	// the observe-first rule, resume believing the operator masked the
	// units. Simulate: reach cutover, mask by hand as the tool would, and
	// leave state at the point just after mask_observed was written.
	if code, out := h.run("y\ny\n"+beneficiary+"\n"+nodeName+"\n8\nnope\n", h.normalOpts()); code != 1 {
		t.Fatalf("setup exit %d:\n%s", code, out)
	}
	st := h.d.Store()
	st.Set("premasked_units", "")
	st.Set("mask_observed", "1")
	h.mock("systemctl", "mask", "monad-bft", "monad-execution", "monad-rpc")
	code, out := h.run("STOPPED\ny\n", Options{Resume: true, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Services masked for the swap", "Services started", "VALIDATOR PROMOTION COMPLETE")
	for _, u := range []string{"monad-bft", "monad-execution", "monad-rpc"} {
		if o, _ := exec.Command("systemctl", "is-enabled", u).Output(); strings.TrimSpace(string(o)) == "masked" {
			t.Errorf("%s left masked", u)
		}
	}
}

func TestSecondRunRefusedWithoutTouchingState(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	if err := h.d.Secure(os.Geteuid(), h.p.BackupRoot); err != nil {
		t.Fatal(err)
	}
	lock, err := h.d.AcquireLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Another monad-failover run is already in progress on this host.")
	if h.stateExists() {
		t.Error("state written by the refused run")
	}
	h.assertServicesUntouched()
}

func TestLeftoverStateOffersResumeAndExitsCleanlyWhenDeclined(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	if code, _ := h.run("y\ny\n0xbad\n", h.normalOpts()); code != 1 {
		t.Fatal("setup")
	}
	if h.stateValue("last_step") != "4" {
		t.Fatalf("setup state %q", h.stateValue("last_step"))
	}
	code, out := h.run("n\n", h.normalOpts())
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Previous run stopped at step 4", "Use: monad-failover --resume")
	if !h.stateExists() {
		t.Error("state cleared although the operator declined")
	}
	// accepting clears it and continues fresh
	code, out = h.run("y\n"+normalStdin(beneficiary, nodeName, "8"), h.normalOpts())
	if code != 0 {
		t.Fatalf("fresh exit %d:\n%s", code, out)
	}
	expect(t, out, "VALIDATOR PROMOTION COMPLETE")
}

func TestFoundationSuggestionAcceptedWithEnter(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	code, out := h.run(normalStdin(beneficiary, nodeName, ""), h.normalOpts())
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Suggested for this migration: 8", "new seq_num [8]", "seq_num for this migration: 8", "VALIDATOR PROMOTION COMPLETE")
}

func TestFoundationSequenceAtOrBelowPublishedIsRefused(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	code, out := h.run(normalStdin(beneficiary, nodeName, "7"), h.normalOpts())
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "seq_num 7 is not higher than the 7 already published for this key.", "Use 8 or higher.")
	h.assertServicesUntouched()
}

func TestFoundationNoRecordIsNotTreatedAsZero(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	h.ep.Snapshot = testutil.Snapshot(testutil.SnapshotOpts{NoPeer: true})
	code, out := h.run(normalStdin(beneficiary, nodeName, "1"), h.normalOpts())
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "no name record published for this key yet", "seq_num for this migration: 1", "VALIDATOR PROMOTION COMPLETE")
	reject(t, out, "Last published sequence")
}

func TestFoundationUnreachableFallsBackToManualEntry(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	h.ep.Snapshot = ""
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Could not read a sequence from the Foundation snapshot (snapshot unreachable).", "Enter the value yourself", "VALIDATOR PROMOTION COMPLETE")
	// and a manual value is never checked against a snapshot that was not read
	if code, out := newHarnessNoSnapshot(t).run(normalStdin(beneficiary, nodeName, "1"), Options{KeySourceDir: "", PublicIP: publicIP}); code != 1 && !strings.Contains(out, "seq_num for this migration: 1") {
		t.Errorf("manual seq 1 with no snapshot: exit %d\n%s", code, out)
	}
}

func newHarnessNoSnapshot(t *testing.T) *harness {
	h := newHarness(t)
	h.healthyEnv()
	h.ep.Snapshot = ""
	return h
}

func TestStaleAndMismatchedSnapshotsAreNotUsed(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts testutil.SnapshotOpts
		note string
	}{
		{"stale", testutil.SnapshotOpts{Stale: true}, "snapshot is 240h old"},
		{"duplicate", testutil.SnapshotOpts{Dup: true}, "2 entries matched this key"},
		{"bls mismatch", testutil.SnapshotOpts{BadBLS: true}, "BLS key does not match the snapshot entry"},
		{"other network", testutil.SnapshotOpts{Network: "mainnet"}, "snapshot is for 'mainnet', not testnet"},
		{"wrong chain", testutil.SnapshotOpts{ChainID: "1"}, "snapshot chain_id is '1', expected 10143"},
		{"truncated", testutil.SnapshotOpts{Truncate: true}, "snapshot is not complete JSON"},
		{"no bls", testutil.SnapshotOpts{NoBLS: true}, "snapshot entry has no BLS key to check against"},
	} {
		h := newHarness(t)
		h.healthyEnv()
		h.ep.Snapshot = testutil.Snapshot(tc.opts)
		code, out := h.normalRun()
		if code != 0 {
			t.Fatalf("%s: exit %d:\n%s", tc.name, code, out)
		}
		expect(t, out, "Could not read a sequence from the Foundation snapshot ("+tc.note+").", "VALIDATOR PROMOTION COMPLETE")
	}
}

func TestConfigMissingFieldsAreInsertedIntoTheirTables(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	toml := h.read(h.p.NodeToml)
	toml = strings.NewReplacer("enable_client = false\n", "", "self_auth_port = 8001\n", "", "expand_to_group = false\n", "").Replace(toml)
	os.WriteFile(h.p.NodeToml, []byte(toml), 0o644)
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	got := h.read(h.p.NodeToml)
	if !tomlIn(got, "peer_discovery", "self_auth_port", "8001") || !tomlIn(got, "fullnode_raptorcast", "enable_client", "true") || !tomlIn(got, "statesync", "expand_to_group", "true") {
		t.Errorf("fields not inserted into their tables:\n%s", got)
	}
}

func TestDuplicateRootKeyIsRejectedBeforeCutover(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	toml := strings.Replace(h.read(h.p.NodeToml), "beneficiary = ", "beneficiary = \"0x0000000000000000000000000000000000000000\"\nbeneficiary = ", 1)
	os.WriteFile(h.p.NodeToml, []byte(toml), 0o644)
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Cannot uniquely set")
	h.assertServicesUntouched()
}

func TestSectionScopedUpdatePreservesUnrelatedTable(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	os.WriteFile(h.p.NodeToml, []byte(h.read(h.p.NodeToml)+"\n[unrelated]\nenable_client = false\n"), 0o644)
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !tomlIn(h.read(h.p.NodeToml), "unrelated", "enable_client", "false") {
		t.Error("unrelated table was modified")
	}
}

func TestBlankBeneficiaryShowsKeptValueAndAsks(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	os.WriteFile(h.p.NodeToml, []byte(strings.Replace(h.read(h.p.NodeToml),
		"0x0000000000000000000000000000000000000000", "0xC0FFEE00000000000000000000000000000C0FFEE", 1)), 0o644)
	// blank, then decline
	code, out := h.run("y\ny\n\nn\n", h.normalOpts())
	if code != 1 {
		t.Fatalf("decline exit %d:\n%s", code, out)
	}
	expect(t, out, "Keeping: 0xC0FFEE00000000000000000000000000000C0FFEE", "Aborted — re-run and enter the beneficiary address you want.")
	h.assertServicesUntouched()
	// blank, then accept (the declined run left state at step 4; start clean)
	h.d.Store().Clear()
	code, out = h.run("y\ny\n\ny\n"+nodeName+"\n8\nSTOPPED\ny\n", h.normalOpts())
	if code != 0 {
		t.Fatalf("accept exit %d:\n%s", code, out)
	}
	expect(t, out, "Beneficiary kept: 0xC0FFEE00000000000000000000000000000C0FFEE", "VALIDATOR PROMOTION COMPLETE")
}

func TestExplicitZeroBeneficiaryWarns(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	code, out := h.run(normalStdin("0x0000000000000000000000000000000000000000", nodeName, "8"), h.normalOpts())
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "You entered the ZERO address.")
}

func TestInvalidNodeNameAbortsBeforeServiceChanges(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	code, out := h.run(normalStdin(beneficiary, "bad name;rm", "8"), h.normalOpts())
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "node_name may contain only letters, digits, dot, dash, underscore")
	h.assertServicesUntouched()
}

func TestInvalidSequenceInputsAreRejected(t *testing.T) {
	for _, seq := range []string{"0", "abc", "-1", "99999999999999999"} {
		h := newHarness(t)
		h.healthyEnv()
		code, out := h.run(normalStdin(beneficiary, nodeName, seq), h.normalOpts())
		if code != 1 {
			t.Errorf("seq %q accepted:\n%s", seq, out)
		}
		h.assertServicesUntouched()
	}
}

func TestIPDetectionFailureGivesOverrideHintAndResumeFinishes(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	h.ep.IP = ""
	code, out := h.run("y\ny\n"+beneficiary+"\n"+nodeName+"\n8\n", Options{KeySourceDir: h.p.BackupRoot})
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Could not detect a valid public IPv4 address.", "monad-failover --resume --public-ip <this-server-public-IPv4>")
	reject(t, out, "Public IP:")
	h.assertServicesUntouched()
	code, out = h.run("STOPPED\ny\n", Options{Resume: true, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("resume exit %d:\n%s", code, out)
	}
	expect(t, out, "Resuming from step 6", "Public IP: "+publicIP, "VALIDATOR PROMOTION COMPLETE")
}

func TestPublicIPOverrideIsSigned(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	h.ep.IP = "198.51.100.99"
	code, out := h.run(normalStdin(beneficiary, nodeName, "8"), Options{KeySourceDir: h.p.BackupRoot, PublicIP: "203.0.113.42"})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Public IP: 203.0.113.42")
	if !tomlIn(h.read(h.p.NodeToml), "peer_discovery", "self_address", `"203.0.113.42:8000"`) {
		t.Error("override not written to node.toml")
	}
}

func TestManualIKMEntryPromotes(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	stdin := "y\n2\n" + testutil.SecpIKM + "\n" + testutil.BlsIKM + "\ny\n" + beneficiary + "\n" + nodeName + "\n8\nSTOPPED\ny\n"
	code, out := h.run(stdin, Options{PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Paste the validator IKM hex values. Input is hidden.", "SECP: "+testutil.MockSecp, "VALIDATOR PROMOTION COMPLETE")
	if strings.Contains(h.runLog(), testutil.SecpIKM) {
		t.Error("typed IKM reached the run log")
	}
}

func TestInteractiveBackupDirectoryDefaultsToBackupRoot(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	code, out := h.run("y\n1\n\ny\n"+beneficiary+"\n"+nodeName+"\n8\nSTOPPED\ny\n", Options{PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "backup directory ["+h.p.BackupRoot+"]", "IKM secrets extracted from backup files", "VALIDATOR PROMOTION COMPLETE")
}

func TestCorruptKeyBackupIsRefused(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	os.WriteFile(filepath.Join(h.p.BackupRoot, "secp-backup"), []byte("Keystore secret: nothex\n"), 0o600)
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Could not extract a valid SECP IKM from")
	h.assertServicesUntouched()
}

func TestNotInSyncRefusesToStart(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	t.Setenv("MOCK_STATUS", "syncing")
	code, out := h.normalRun()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Node is syncing. Must be fully synced before promotion.")
	if h.stateExists() {
		t.Error("state written before the sync gate")
	}
}

func TestEnvWithCRLFYieldsExactPassword(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	os.WriteFile(h.p.EnvFile, []byte("KEYSTORE_PASSWORD='testpass'\r\n"), 0o600)
	code, out := h.normalRun()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "VALIDATOR PROMOTION COMPLETE")
}

func TestKeyMismatchDeclinedAborts(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	code, out := h.run("y\nn\n", h.normalOpts())
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Key mismatch — aborting.")
	h.assertServicesUntouched()
	if _, err := os.Stat(h.d.SecpNew); err != nil {
		t.Error("staging key missing: the prompt should follow the import")
	}
	if h.stateValue("secp_pub") != "" {
		t.Error("unconfirmed keys recorded in state")
	}
}

// State written by the 1.9.x shell release has the same layout; a run it
// left at step 3 is picked up from step 4.
func TestStateFromShellReleaseResumes(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	if err := h.d.Secure(os.Geteuid(), h.p.BackupRoot); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(h.p.BackupRoot, "failover-20260911-072052")
	os.MkdirAll(backup, 0o700)
	os.WriteFile(h.d.File, []byte("last_step=3\nnetwork=testnet\nbackup_dir="+backup+"\n"), 0o600)
	code, out := h.run("y\n"+beneficiary+"\n"+nodeName+"\n8\nSTOPPED\ny\n", Options{Resume: true, KeySourceDir: h.p.BackupRoot, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Resuming from step 4", "VALIDATOR PROMOTION COMPLETE")
	reject(t, out, "Config backed up to")
}

func TestResumeWithNothingToResumeStartsFresh(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	code, out := h.run(normalStdin(beneficiary, nodeName, "8"), Options{Resume: true, KeySourceDir: h.p.BackupRoot, PublicIP: publicIP})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "No previous run found. Starting fresh.", "VALIDATOR PROMOTION COMPLETE")
}

func TestTamperedStateIsRefusedNotEvaluated(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	if err := h.d.Secure(os.Geteuid(), h.p.BackupRoot); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(h.root, "pwned")
	os.WriteFile(h.d.File, []byte("last_step=a[$(touch "+marker+")]\n"), 0o600)
	code, out := h.run("", Options{Resume: true, PublicIP: publicIP})
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "'last_step' holds an unexpected value")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("state content was executed")
	}
	// the same on a non-resume run
	code, out = h.run("", h.normalOpts())
	if code != 1 {
		t.Fatalf("fresh exit %d:\n%s", code, out)
	}
	expect(t, out, "'last_step' holds an unexpected value")
}

func TestLegacyStateIsRefusedNeverMigrated(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	legacy := filepath.Join(h.p.MonadHome, ".monad-failover")
	os.MkdirAll(legacy, 0o700)
	os.WriteFile(filepath.Join(legacy, "state"), []byte("last_step=6\n"), 0o600)
	code, out := h.run("", Options{Resume: true})
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "Found state from an older version at "+filepath.Join(legacy, "state"), "not migrated automatically")
	if _, err := os.Stat(filepath.Join(legacy, "state")); err != nil {
		t.Error("legacy state removed")
	}
	if h.stateExists() {
		t.Error("legacy state migrated")
	}
}

func TestDryRunOnHealthyEnvironmentPassesAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	before := h.liveSHAs()
	code, out := h.dryRun("")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "DRY RUN", "systemctl", "monad-keystore", "KEYSTORE_PASSWORD set", "in-sync (block difference: 0)",
		"RPC EXPOSURE CHECK", "valid IKM format; validator identity NOT verified", "Preflight passed")
	if h.liveSHAs() != before {
		t.Error("files changed")
	}
	if _, err := os.Stat(h.d.Root); err == nil {
		t.Error("state directory created by the dry run")
	}
	if _, err := os.Stat(h.p.LogDir); err == nil {
		t.Error("log directory created by the dry run")
	}
	h.assertServicesUntouched()
}

func TestDryRunFailsOnEmptyEnvironment(t *testing.T) {
	h := newHarness(t)
	code, out := h.dryRun("")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "missing: "+h.p.NodeToml, "missing: "+h.p.EnvFile, "Preflight failed")
}

func TestDryRunWarnsOnBackupWithoutIKM(t *testing.T) {
	h := newHarness(t)
	h.healthyEnv()
	os.WriteFile(filepath.Join(h.p.BackupRoot, "bls-backup"), []byte("nothing useful\n"), 0o600)
	code, out := h.dryRun("")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	expect(t, out, "exists but contains no valid IKM", "Preflight passed")
}
