package dataset

// LoadErrorCode is the failure code of a built-in dataset that could not be
// loaded. It is the only place the code is spelled; the run status carries it
// (proto.BenchmarkStatus.Code) and the console renders it from the code.
const LoadErrorCode = "BENCHMARK_DATASET_LOAD_FAILED"

// LoadError is a built-in dataset that could not be read or parsed. A run
// that meets it stops before any request: it never falls back to random
// prompts, which would measure a different workload under the same name.
type LoadError struct {
	// Dataset is the built-in dataset kind, e.g. "multiturn".
	Dataset string
	Err     error
}

func (e *LoadError) Error() string {
	return "load the built-in " + e.Dataset + " dataset: " + e.Err.Error()
}

func (e *LoadError) Unwrap() error { return e.Err }

// Code is the stable failure code (LoadErrorCode).
func (e *LoadError) Code() string { return LoadErrorCode }

// Params are the code's interpolation parameters.
func (e *LoadError) Params() map[string]any { return map[string]any{"dataset": e.Dataset} }
