package inferenceapi

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

func TestMetadataToOperations(t *testing.T) {
	tests := []struct {
		name, config, readme, want string
	}{
		{"ranking with generation backbone", `{"architectures":["Qwen3ForCausalLM"]}`, "---\npipeline_tag: text-ranking\n---\n", OperationRerank},
		{"reranker tag", `{}`, "\n---\npipeline_tag: text-classification\ntags: [reranker]\n---\n", OperationRerank},
		{"cross encoder label map", `{"architectures":["XLMRobertaForSequenceClassification"],"id2label":{"0":"LABEL_0"}}`, "---\ntags: [sentence-transformers]\n---\n", OperationRerank},
		{"cross encoder label count", `{"architectures":["BertForSequenceClassification"],"num_labels":1}`, "---\ntags: [sentence-transformers]\n---\n", OperationRerank},
		{"multiclass is not ranking", `{"architectures":["BertForSequenceClassification"],"num_labels":2,"id2label":{"0":"LABEL_0"}}`, "---\ntags: [sentence-transformers]\n---\n", ""},
		{"scalar regression is not ranking", `{"architectures":["BertForSequenceClassification"],"num_labels":1}`, "---\npipeline_tag: text-classification\n---\n", ""},
		{"unknown label count", `{"architectures":["BertForSequenceClassification"]}`, "---\ntags: [sentence-transformers]\n---\n", ""},
		{"feature extraction", `{}`, "---\npipeline_tag: feature-extraction\n---\n", OperationEmbeddings},
		{"sentence similarity", `{}`, "---\npipeline_tag: sentence-similarity\n---\n", OperationEmbeddings},
		{"feature extraction tag", `{}`, "---\ntags: [feature-extraction]\n---\n", OperationEmbeddings},
		{"ranking tag wins generic embedding tag", `{}`, "---\ntags: [reranker, feature-extraction]\n---\n", OperationRerank},
		{"declared task wins tag", `{}`, "---\npipeline_tag: text-generation\ntags: [feature-extraction]\n---\n", OperationChat},
		{"CRLF and BOM", `{}`, "\ufeff---\r\npipeline_tag: text-ranking\r\n---\r\n", OperationRerank},
		{"README prose is not a declaration", `{}`, "# Reranker\nExample:\n---\npipeline_tag: text-ranking\n---\n", ""},
		{"truncated card", `{}`, "---\npipeline_tag: text-ranking\n", ""},
		{"malformed card", `{}`, "---\ntags: [broken\n---\n", ""},
		{"text to image", `{}`, "---\npipeline_tag: text-to-image\n---\n", OperationImageGenerate},
		{"image to image edits a source image", `{}`, "---\npipeline_tag: image-to-image\n---\n", OperationImageEdit},
		{"ordinary chat", `{"architectures":["Qwen2ForCausalLM"]}`, "# Chat", OperationChat},
		{"no metadata", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OperationsForTasks(modelmetadata.InferTasks(modelmetadata.Documents{Config: tt.config, README: tt.readme}).Tasks)
			if (tt.want == "" && len(got) != 0) || (tt.want != "" && (len(got) != 1 || got[0].Type != tt.want)) {
				t.Fatalf("operations = %+v, want %s", got, tt.want)
			}
		})
	}
}

// Registration stores OperationsForTasks unnormalized, and a template may
// replace that value only while store recognizes it as such a placeholder. A
// task added here without the store's literal list following would leave every
// checkpoint with that task stuck on a contract with no endpoint or inputs.
func TestRegistrationInferredOperationsAreTemplatePlaceholders(t *testing.T) {
	tasks := []string{
		"text-ranking", "feature-extraction", "text-generation", "text2text-generation", "image-text-to-text",
		"automatic-speech-recognition", "text-to-speech", "text-to-audio", "text-to-image", "image-to-image",
		"text-to-video", "image-to-video", "image-text-to-video",
	}
	for _, task := range tasks {
		operations := OperationsForTasks([]string{task})
		if len(operations) != 1 {
			t.Fatalf("task %s inferred %d operations, want 1", task, len(operations))
		}
		raw, err := json.Marshal(operations)
		if err != nil {
			t.Fatal(err)
		}
		if !store.IsDefaultTemplateOperations(string(raw)) {
			t.Errorf("task %s registers %s, which a template cannot replace", task, raw)
		}
	}
	raw, err := json.Marshal(OperationsForTasks([]string{"text-to-image", "image-to-image"}))
	if err != nil {
		t.Fatal(err)
	}
	if !store.IsDefaultTemplateOperations(string(raw)) {
		t.Fatalf("a text-to-image and image-to-image checkpoint registers %s; it must stay a template placeholder", raw)
	}
}
