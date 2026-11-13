package api

import (
	"math/rand/v2"
)

const servedNameSuffixAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// withServedNameSuffix gives a name a random four-character suffix, the same
// shape the deploy form proposes, so a copy of a deployment does not ask for
// the served name its original holds.
func withServedNameSuffix(name string) string {
	suffix := make([]byte, 4)
	for i := range suffix {
		suffix[i] = servedNameSuffixAlphabet[rand.IntN(len(servedNameSuffixAlphabet))]
	}
	return name + "-" + string(suffix)
}
