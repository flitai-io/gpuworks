package inferenceapi

// OperationsForTasks translates parsed task names into the platform contract.
// Unknown tasks deliberately produce an empty contract, never a chat default.
func OperationsForTasks(tasks []string) []Operation {
	out := []Operation{}
	seen := map[string]bool{}
	for _, task := range tasks {
		var operation string
		switch task {
		case "text-ranking":
			operation = OperationRerank
		case "feature-extraction":
			operation = OperationEmbeddings
		case "decision":
			operation = OperationDecide
		case "text-generation", "text2text-generation", "image-text-to-text":
			operation = OperationChat
		case "automatic-speech-recognition":
			operation = OperationAudioTranscribe
		case "text-to-speech", "text-to-audio":
			operation = OperationAudioSpeech
		case "text-to-image":
			operation = OperationImageGenerate
		case "image-to-image":
			// Image-to-image checkpoints take a source image, which only the
			// multipart edit route carries.
			operation = OperationImageEdit
		case "text-to-video", "image-to-video", "image-text-to-video":
			operation = OperationVideoGenerate
		}
		if operation != "" && !seen[operation] {
			out = append(out, Operation{Type: operation})
			seen[operation] = true
		}
	}
	return out
}
