package aihelper

import "testing"

func TestDeepSeekPresetUsesOfficialDefaults(t *testing.T) {
	p, ok := Presets["deepseek"]
	if !ok {
		t.Fatal("deepseek preset is missing")
	}
	if p.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("base URL = %q, want https://api.deepseek.com", p.BaseURL)
	}
	if p.Model != "deepseek-flash" || !p.ImageInput {
		t.Fatalf("model = %q image=%v, want deepseek-flash with image input", p.Model, p.ImageInput)
	}
}

// The dictionary ends with the custom choice, which fills nothing and is not a
// preset a create or a patch fills from.
func TestPresetListEndsWithCustom(t *testing.T) {
	last := PresetList[len(PresetList)-1]
	if last != (Preset{Name: PresetCustom}) {
		t.Fatalf("last preset = %+v", last)
	}
	if _, ok := Presets[PresetCustom]; ok {
		t.Fatal("custom fills a service")
	}
	if len(Presets) != len(PresetList)-1 {
		t.Fatalf("presets = %d, list = %d", len(Presets), len(PresetList))
	}
}
