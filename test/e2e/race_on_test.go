//go:build e2e && race

package e2e

// raceEnabled: the test binary runs with the race detector, so the mycel
// binary under test is built with -race too.
const raceEnabled = true
