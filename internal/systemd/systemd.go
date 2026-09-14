// Package systemd drives the three Monad units through systemctl.
package systemd

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
)

// Units are the services a migration manages, in the order the tool names them.
var Units = []string{"monad-bft", "monad-execution", "monad-rpc"}

func run(stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// IsEnabled returns the first line systemctl prints ("masked", "enabled",
// ...) or "" when the query fails.
func IsEnabled(unit string) string {
	var out bytes.Buffer
	_ = run(&out, io.Discard, "is-enabled", unit)
	line, _, _ := strings.Cut(out.String(), "\n")
	return strings.TrimSpace(line)
}

// IsMasked reports whether the unit is masked.
func IsMasked(unit string) bool { return IsEnabled(unit) == "masked" }

// IsActive reports whether the unit is active.
func IsActive(unit string) bool {
	return run(io.Discard, io.Discard, "is-active", "--quiet", unit) == nil
}

// Mask masks all units; the result is verified by the caller per unit.
func Mask(units ...string) {
	_ = run(io.Discard, io.Discard, append([]string{"mask"}, units...)...)
}

// Unmask unmasks one unit; verified by the caller.
func Unmask(unit string) { _ = run(io.Discard, io.Discard, "unmask", unit) }

// Stop stops all units; the caller checks is-active afterwards.
func Stop(stdout io.Writer, units ...string) {
	_ = run(stdout, io.Discard, append([]string{"stop"}, units...)...)
}

// Enable enables the units. Failure is ignored: enabling is best effort,
// starting is what gets checked.
func Enable(units ...string) {
	_ = run(io.Discard, io.Discard, append([]string{"enable"}, units...)...)
}

// Start starts the units, passing systemctl's own output through.
func Start(stdout, stderr io.Writer, units ...string) error {
	return run(stdout, stderr, append([]string{"start"}, units...)...)
}
