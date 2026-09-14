package harden

import (
	"os"
	"syscall"
	"testing"
)

func TestApply(t *testing.T) {
	Apply(os.Geteuid())
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &lim); err != nil {
		t.Fatal(err)
	}
	if lim.Cur != 0 || lim.Max != 0 {
		t.Errorf("core limit %+v", lim)
	}
	if Dumpable() != 0 {
		t.Errorf("dumpable = %d", Dumpable())
	}
}
