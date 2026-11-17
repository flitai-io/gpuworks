//go:build !race

package obsingest

// raceEnabled reports whether this test binary was built with the race
// detector. See race_on_test.go for why this is a local copy rather than an
// import of internal/testenv's own RaceEnabled.
const raceEnabled = false
