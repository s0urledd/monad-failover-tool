package paths

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/s0urledd/monad-failover-tool/internal/ui"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MONAD_HOME", "BACKUP_ROOT", "LOG_DIR", "MF_STATE_DIR", "MF_ALLOW_NONROOT",
		"FOUNDATION_DATA_BASE", "FOUNDATION_MAX_AGE", "MF_HEALTH_WAIT", "MF_SYNC_WAIT", "MF_IP_URL", "MF_UPTIME_API_BASE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	p, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if p.MonadHome != "/home/monad" || p.NodeToml != "/home/monad/monad-bft/config/node.toml" ||
		p.EnvFile != "/home/monad/.env" || p.BackupRoot != "/opt/monad/backup" || p.LogDir != "/opt/monad/failover-logs" ||
		p.FoundationBase != DefaultFoundationBase || p.UptimeMainnet != DefaultUptimeMainnet ||
		p.HealthWait.Seconds() != 5 || p.SyncWait.Seconds() != 120 || p.FoundationMaxAge.Seconds() != 86400 {
		t.Errorf("defaults: %+v", p)
	}
	if p.Sandbox {
		t.Error("sandbox without MF_ALLOW_NONROOT")
	}
}

// The test-only overrides could redirect trusted state or the values the run
// shows and signs, so outside the sandbox they are refused, never ignored.
func TestTestOnlyOverridesRefusedOutsideSandbox(t *testing.T) {
	for _, k := range []string{"MF_STATE_DIR", "MF_IP_URL", "MF_UPTIME_API_BASE"} {
		clearEnv(t)
		t.Setenv(k, "/tmp/x")
		_, err := FromEnv()
		var f *ui.Fatal
		if !errors.As(err, &f) || !strings.Contains(f.Msg, k+" is only honoured by the unprivileged test suite.") {
			t.Errorf("%s: got %v", k, err)
		}
	}
}

func TestSandboxHonoursOverrides(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the sandbox is unprivileged by definition")
	}
	clearEnv(t)
	t.Setenv("MF_ALLOW_NONROOT", "1")
	t.Setenv("MF_STATE_DIR", "/tmp/state")
	t.Setenv("MF_IP_URL", "http://127.0.0.1:1/ip")
	t.Setenv("MF_UPTIME_API_BASE", "http://127.0.0.1:1/uptime")
	t.Setenv("MF_HEALTH_WAIT", "0")
	t.Setenv("MF_SYNC_WAIT", "0")
	p, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Sandbox || p.StateDirOverride != "/tmp/state" || p.IPURL != "http://127.0.0.1:1/ip" ||
		p.UptimeMainnet != "http://127.0.0.1:1/uptime" || p.UptimeTestnet != "http://127.0.0.1:1/uptime" ||
		p.HealthWait != 0 || p.SyncWait != 0 {
		t.Errorf("sandbox: %+v", p)
	}
}

func TestOperatorOverridesAlwaysApply(t *testing.T) {
	clearEnv(t)
	t.Setenv("MONAD_HOME", "/srv/monad")
	t.Setenv("BACKUP_ROOT", "/srv/backup")
	t.Setenv("FOUNDATION_DATA_BASE", "https://mirror.example/validator-data")
	t.Setenv("FOUNDATION_MAX_AGE", "3600")
	p, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if p.SecpKey != "/srv/monad/monad-bft/config/id-secp" || p.BackupRoot != "/srv/backup" ||
		p.FoundationBase != "https://mirror.example/validator-data" || p.FoundationMaxAge.Seconds() != 3600 {
		t.Errorf("overrides: %+v", p)
	}
}

func TestBadDurationsAreRefused(t *testing.T) {
	for _, k := range []string{"FOUNDATION_MAX_AGE", "MF_HEALTH_WAIT", "MF_SYNC_WAIT"} {
		clearEnv(t)
		t.Setenv(k, "soon")
		if _, err := FromEnv(); err == nil {
			t.Errorf("%s=soon accepted", k)
		}
	}
}
