package inferenceapi

import "testing"

func TestUsageForPathFollowsTheRegisteredEndpoints(t *testing.T) {
	chat, ok := UsageForPath("/v1/chat/completions")
	if !ok || chat.Prompt != "prompt_tokens" || chat.Completion != "completion_tokens" || len(chat.StreamText) == 0 {
		t.Fatalf("chat usage = %+v, %v", chat, ok)
	}
	if embeddings, ok := UsageForPath("/v1/embeddings"); !ok || embeddings.Completion != "" || len(embeddings.StreamText) != 0 {
		t.Fatalf("embeddings usage = %+v, %v", embeddings, ok)
	}
	// A decision generates nothing: its usage.output_tokens counts answered
	// questions, so only the input is read.
	if decide, ok := UsageForPath("/v1/systemone"); !ok || decide.Prompt != "input_tokens" || decide.Completion != "" || len(decide.StreamText) != 0 {
		t.Fatalf("decision usage = %+v, %v", decide, ok)
	}
	for _, path := range []string{"/v1/audio/speech", "/v1/images/generations", "/v1/chat", "", "/v1/chat/completions/"} {
		if _, ok := UsageForPath(path); ok {
			t.Fatalf("%q has usage", path)
		}
	}
	// Every operation with usage is a registered one, and every endpoint it
	// may be served on is covered.
	for operation := range usages {
		definition, ok := Lookup(operation)
		if !ok {
			t.Fatalf("usage for unregistered %s", operation)
		}
		for _, endpoint := range definition.AllowedEndpoints {
			if _, ok := UsageForPath(endpoint); !ok {
				t.Fatalf("%s endpoint %s not covered", operation, endpoint)
			}
		}
	}
}
