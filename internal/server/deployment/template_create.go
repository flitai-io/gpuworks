package deployment

// templateInferenceConfig is the declaration whose conditional initialization
// joins a template deployment's create transaction.
type templateInferenceConfig struct {
	modelID         uint
	operations      string
	inputModalities string
}
