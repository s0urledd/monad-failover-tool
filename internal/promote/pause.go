//go:build !reboottest

package promote

// pauseAfterSecp is a no-op in release builds. The reboot test in
// docs/reboot-test.md builds with -tags reboottest to get a 30 s window
// between the first and second file placement.
func pauseAfterSecp(*Run) {}
