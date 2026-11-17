//go:build race

package obsingest

// raceEnabled reports whether this test binary was built with the race
// detector. Mirrors internal/testenv's own RaceEnabled (kept as a separate,
// tiny copy here rather than importing testenv: testenv imports the app,
// which imports this package, so importing it from this package's own tests
// would be an import cycle). Wall-clock assertions relax their bound under race, since
// the instrumentation itself (and, on a shared/loaded host, contention from
// other processes) can dominate the measurement far more than the code path
// under test.
const raceEnabled = true
