package inferenceapi

import "regexp"

// Catalog declarations cannot name arbitrary URLs or node/server paths. A
// full commit keeps a later repository update from changing a declared source.
var catalogTokenizerReference = regexp.MustCompile(`^(hf|ms)://[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*@[0-9a-fA-F]{40}$`)
