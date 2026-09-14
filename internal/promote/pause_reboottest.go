//go:build reboottest

package promote

import "time"

// pauseAfterSecp holds the run between the SECP and BLS placements so the
// reboot test can interrupt a half-swapped node. Never part of a release
// build: it exists only behind the reboottest build tag.
func pauseAfterSecp(r *Run) {
	r.c.Warn("reboottest build: pausing 30s after the SECP placement")
	r.sleep(30 * time.Second)
}
