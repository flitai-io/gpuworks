package accelerator

import (
	"os"
	"regexp"
	"testing"
)

// The PCI address has to win over the UUID: Ascend and Hygon probes build
// the UUID out of the very device index a binding must survive.
func TestCardIdentPrefersThePCIAddressOverASynthesizedUUID(t *testing.T) {
	ascendAt3 := CardIdent("ascend-3", "0000:c1:00.0")
	ascendRenumberedTo1 := CardIdent("ascend-1", "0000:C1:00.0")
	if ascendAt3 != ascendRenumberedTo1 {
		t.Fatalf("the same card read as %q then %q", ascendAt3, ascendRenumberedTo1)
	}
	if other := CardIdent("ascend-3", "0000:c2:00.0"); other == ascendAt3 {
		t.Fatalf("two cards share the identity %q", other)
	}
}

func TestCardIdentNormalizesDomainDigitsAndCase(t *testing.T) {
	if long, short := CardIdent("", "00000000:7E:00.0"), CardIdent("", "0000:7e:00.0"); long != short {
		t.Fatalf("%q != %q", long, short)
	}
}

func TestCardIdentFallsBackToUUIDThenToNothing(t *testing.T) {
	if got := CardIdent("GPU-5fe053f2", "  "); got != "uuid:GPU-5fe053f2" {
		t.Fatalf("identity without a PCI address = %q", got)
	}
	if got := CardIdent("", ""); got != "" {
		t.Fatalf("identity of an unidentified card = %q, want empty so callers keep using indexes", got)
	}
	// A UUID and a PCI address of the same spelling must not collide.
	if CardIdent("0000:7e:00.0", "") == CardIdent("", "0000:7e:00.0") {
		t.Fatal("a UUID that looks like a PCI address collides with one")
	}
}

// A tie in healthSeverity hands the choice to the order the caller passed its
// codes in: MostSevereHealth keeps the first of two equal ranks, so the same
// card would report different findings depending on which call site asked.
// That is exactly what the table is for, so every rank must be distinct.
func TestHealthSeverityIsATotalOrder(t *testing.T) {
	byRank := map[int]string{}
	for _, code := range healthCodesDeclaredInSource(t) {
		rank := healthSeverity[code]
		if other, taken := byRank[rank]; taken {
			t.Errorf("%q and %q both sort at %d; one of them has to be more worth reading than the other", code, other, rank)
		}
		byRank[rank] = code
	}
	// The order within each band, stated as the behaviour rather than as
	// numbers: a named finding beats an absent diagnostic, and a card the
	// runtime cannot use beats both.
	for _, tc := range [][2]string{
		{HealthVendorWarning, HealthTelemetryUnavailable},
		{HealthMemoryRepairPending, HealthVendorWarning},
		{HealthRuntimeUnusable, HealthMemoryRepairPending},
		{HealthInventoryInvalid, HealthVendorFault},
		{HealthResetRequired, HealthInventoryInvalid},
		{HealthDriverInitFailed, HealthCLIUnanswered},
	} {
		if got := MostSevereHealth(tc[1], tc[0]); got != tc[0] {
			t.Errorf("MostSevereHealth(%q, %q) = %q, want %q", tc[1], tc[0], got, tc[0])
		}
	}
}

// A code absent from healthSeverity sorts below every other, so
// MostSevereHealth can never choose it and a card would keep a milder
// finding than the truth. Every declared code must be ordered.
func TestEveryHealthCodeHasASeverity(t *testing.T) {
	// Read from the source rather than listed here. A hand-kept list is a
	// list somebody forgets: vendor_cli_unanswered was added to health.go
	// and not to this test, so it sorted below every other code and lost
	// to "errors that were already handled".
	codes := healthCodesDeclaredInSource(t)
	if len(codes) < 11 {
		t.Fatalf("found only %d health codes in health.go; the scan is broken", len(codes))
	}
	for _, code := range codes {
		if healthSeverity[code] == 0 {
			t.Errorf("code %q has no severity, so it can never be a card's reported finding", code)
		}
	}
	// A card the runtime cannot use outranks the counters a vendor tool
	// reports about a card it believes is working.
	if got := MostSevereHealth(HealthMemoryRepairPending, HealthRuntimeUnusable); got != HealthRuntimeUnusable {
		t.Fatalf("most severe = %q, want the runtime finding", got)
	}
	// A card the tool does not list at all outranks everything: no other
	// finding about it can exist.
	if got := MostSevereHealth(HealthRuntimeUnusable, HealthDriverInitFailed); got != HealthDriverInitFailed {
		t.Fatalf("most severe = %q, want the uninitialized card", got)
	}
	// Both are errors, so neither is schedulable.
	for _, code := range []string{HealthRuntimeUnusable, HealthDriverInitFailed} {
		if HealthSchedulable(HealthLevel(code)) {
			t.Fatalf("a card at %q is schedulable", code)
		}
	}
}

// healthCodesDeclaredInSource is every Health* code const health.go
// declares, read from the file so no new one can escape the checks above.
func healthCodesDeclaredInSource(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("health.go")
	if err != nil {
		t.Fatal(err)
	}
	// `HealthSomething = "some_code"`, which is how every code is written.
	declared := regexp.MustCompile(`(?m)^\s*Health[A-Za-z]+\s*=\s*"([a-z_]+)"`)
	var codes []string
	for _, m := range declared.FindAllStringSubmatch(string(source), -1) {
		// The three levels are not findings; they are what a finding
		// sorts a card into.
		switch m[1] {
		case "OK", "Warning", "Error", "ok", "warning", "error":
			continue
		}
		codes = append(codes, m[1])
	}
	return codes
}

// One PCI function has one address however a tool prints its domain, and
// two functions never share one. Cutting every address to its last twelve
// characters did the first and broke the second for a domain past 0xffff.
func TestNormalizeBDFNamesEachFunctionOnce(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"00000000:7E:00.0", "0000:7e:00.0"},
		{" 0000:7e:00.0 ", "0000:7e:00.0"},
		{"10000:01:00.0", "10000:01:00.0"},
		{"00010000:01:00.0", "10000:01:00.0"},
		{"7e:00.0", "7e:00.0"},
		{"", ""},
	} {
		if got := NormalizeBDF(tc.in); got != tc.want {
			t.Errorf("NormalizeBDF(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if NormalizeBDF("10000:01:00.0") == NormalizeBDF("0000:01:00.0") {
		t.Fatal("cards in different PCI domains share an address")
	}
}
