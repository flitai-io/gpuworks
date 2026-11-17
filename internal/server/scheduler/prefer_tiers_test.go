package scheduler

import "testing"

// planPreferring may try 2^n tiers; the table stays small enough that the
// worst case is a handful of in-memory plans.
func TestCardPreferencesStayWithinTheTierBudget(t *testing.T) {
	if len(cardPreferences) > maxCardPreferences {
		t.Fatalf("%d card preferences, budget %d", len(cardPreferences), maxCardPreferences)
	}
	for i, pref := range cardPreferences {
		if pref.avoid == nil || pref.reason == "" || pref.params == nil {
			t.Fatalf("preference %d is incomplete: %+v", i, pref)
		}
	}
}
