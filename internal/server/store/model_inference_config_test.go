package store

import (
	"path/filepath"
	"sync"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestOperationsUnset(t *testing.T) {
	for raw, want := range map[string]bool{
		"":                            true,
		"  \n":                        true,
		"[]":                          true,
		" [ ] ":                       true,
		"[\n]":                        true,
		"[\t]":                        true,
		`[{"type":"openai.chat"}]`:    false,
		`[ {"type":"openai.chat"} ]`:  false,
		"{not json":                   false,
		`{"type":"openai.chat"}`:      false,
		"null":                        true,
		`[{"type":"openai.invalid"}]`: false,
	} {
		if got := OperationsUnset(raw); got != want {
			t.Errorf("OperationsUnset(%q)=%v want %v", raw, got, want)
		}
	}
}

// Every spelling of "no operations" is filled by the source's declaration, and
// the SQL condition agrees with OperationsUnset on each of them.
func TestInitModelInferenceConfigFillsEmptyOperations(t *testing.T) {
	declared := `[{"type":"openai.chat"}]`
	for name, stored := range map[string]string{
		"empty": "", "empty array": "[]", "null": "null", "spaced array": "[ ]", "newline array": "[\n]", "blank": "  ",
	} {
		t.Run(name, func(t *testing.T) {
			st := newTestStore(t)
			model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "acme/" + name, Revision: "main", Name: name, Operations: stored})
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.InitModelInferenceConfig(model.ID, declared, "")
			if err != nil || got.Operations != declared {
				t.Fatalf("operations=%q err=%v want %q", got.Operations, err, declared)
			}
		})
	}
}

// Operations that are present stay, and a value a concurrent edit writes
// between the caller's read and Init's UPDATE wins: the condition is in the
// UPDATE, not in the caller's view of the row.
func TestInitModelInferenceConfigKeepsNonEmptyOperations(t *testing.T) {
	st := newTestStore(t)
	model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "acme/kept", Revision: "main", Name: "kept", Operations: "[]", InputModalities: `["text"]`})
	if err != nil {
		t.Fatal(err)
	}
	// The caller saw [] ...
	if !OperationsUnset(model.Operations) {
		t.Fatalf("precondition: %q", model.Operations)
	}
	// ... then an edit lands a real value before the deploy's write.
	edited := `[{"type":"openai.embeddings"}]`
	if _, err := st.SetModelInferenceConfig(model.ID, edited, `["text"]`); err != nil {
		t.Fatal(err)
	}
	got, err := st.InitModelInferenceConfig(model.ID, `[{"type":"openai.chat"}]`, `["text","image"]`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Operations != edited || got.InputModalities != `["text"]` {
		t.Fatalf("a concurrent value was overwritten: operations=%q modalities=%q", got.Operations, got.InputModalities)
	}
}

func TestInitTemplateModelInferenceConfigUsesDeclaredVideoContract(t *testing.T) {
	const declared = `[{"inputs":[{"name":"size","kind":"select","options":["640x360"]}],"type":"openai.video.generate","endpoint":"/v1/videos/sync","mode":"sync","request_encoding":"multipart"}]`
	const explicitJSON = `[{"inputs":[{"name":"size","kind":"select","options":["640x360"]}],"type":"openai.video.generate","endpoint":"/v1/videos/sync","mode":"sync","request_encoding":"json"}]`
	for _, tc := range []struct {
		name, stored, want string
	}{
		{"inferred type only", `[{"type":"openai.video.generate"}]`, declared},
		{"inferred with null inputs", `[{"inputs":null,"type":"openai.video.generate"}]`, declared},
		{"explicit JSON is preserved", explicitJSON, explicitJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			model, err := st.UpsertModel(&Model{
				Source: "modelscope", SourceID: "acme/video", Revision: "main", Name: "video",
				Operations: tc.stored, InputModalities: `["text","image"]`,
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.InitTemplateModelInferenceConfig(model.ID, declared, `["text"]`)
			if err != nil {
				t.Fatal(err)
			}
			if got.Operations != tc.want || got.InputModalities != `["text","image"]` {
				t.Fatalf("operations=%q modalities=%q want operations=%q and original modalities", got.Operations, got.InputModalities, tc.want)
			}
			persisted, err := st.GetModel(model.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Operations != tc.want {
				t.Fatalf("persisted operations=%q want %q", persisted.Operations, tc.want)
			}
		})
	}
}

func TestInitTemplateModelInferenceConfigLetsAConcurrentEditWin(t *testing.T) {
	for _, tc := range []struct {
		name, stored, declared, edited string
	}{
		{
			name: "chat", stored: legacyChatPlaceholderFixture,
			declared: `[{"type":"openai.embeddings"}]`,
			edited:   `[{"inputs":[],"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
		},
		{
			name: "video", stored: `[{"inputs":null,"type":"openai.video.generate"}]`,
			declared: `[{"inputs":[],"type":"openai.video.generate","endpoint":"/v1/videos/sync","mode":"sync","request_encoding":"multipart"}]`,
			edited:   `[{"inputs":[],"type":"openai.video.generate","endpoint":"/v1/videos/sync","mode":"sync","request_encoding":"json"}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "template-config-cas.db")
			st, err := Open(path, bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			other, err := Open(path, bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			model, err := st.UpsertModel(&Model{
				Source: "modelscope", SourceID: "acme/concurrent", Revision: "main", Name: "concurrent",
				Operations: tc.stored,
			})
			if err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			if err := st.db.Callback().Query().After("gorm:query").Register("test:edit_template_config_after_read", func(tx *gorm.DB) {
				if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "Model" {
					return
				}
				once.Do(func() {
					// The template read its inferred placeholder; another request
					// commits before the conditional UPDATE. The old-value guard
					// must preserve that edit.
					if _, err := other.SetModelInferenceConfig(model.ID, tc.edited, `["text"]`); err != nil {
						tx.AddError(err)
					}
				})
			}); err != nil {
				t.Fatal(err)
			}
			defer st.db.Callback().Query().Remove("test:edit_template_config_after_read")
			got, err := st.InitTemplateModelInferenceConfig(model.ID, tc.declared, `["image"]`)
			if err != nil {
				t.Fatal(err)
			}
			if got.Operations != tc.edited || got.InputModalities != `["text"]` {
				t.Fatalf("concurrent edit was overwritten: operations=%q modalities=%q", got.Operations, got.InputModalities)
			}
		})
	}
}
