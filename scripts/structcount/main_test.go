package main

import "testing"

// The fixture holds every way of hiding state the line-matching ratchet let
// through: several names on one line, a lock by pointer, sync.Map, a map type
// of the package, fields wrapped in an anonymous struct, RPC cases with
// several values or moved to a helper switch, methods on a value receiver.
// Methods in test files and a named subsystem's own fields do not count.
func TestCountSeesHiddenState(t *testing.T) {
	for _, tc := range []struct {
		measure string
		want    int
	}{
		// aMu bMu cMu m1 m2 cache seen nested(+dMu byID) images Mutex
		{"fields", 12},
		// aMu bMu cMu nested.dMu Mutex(embedded)
		{"mutexes", 5},
		// m1 m2 cache seen nested.byID
		{"maps", 5},
		// OnRPC more
		{"methods", 2},
		// Ping Doctor Smoke
		{"rpcs", 3},
	} {
		got, err := count("testdata/root", "Root", tc.measure)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s = %d, want %d", tc.measure, got, tc.want)
		}
	}
}

func TestCountRefusesWhatItCannotMeasure(t *testing.T) {
	for _, tc := range []struct{ dir, typ, measure string }{
		{"testdata/root", "Missing", "fields"},
		{"testdata/root", "uuidSet", "fields"},
		{"testdata/root", "Root", "lines"},
		{"testdata/none", "Root", "fields"},
	} {
		if _, err := count(tc.dir, tc.typ, tc.measure); err == nil {
			t.Errorf("count(%q, %q, %q) succeeded", tc.dir, tc.typ, tc.measure)
		}
	}
}
