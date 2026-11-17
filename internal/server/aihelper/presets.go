package aihelper

// Preset is a provider the platform knows an address and a starting model for.
// ImageInput is whether that starting model is known to accept images; it only
// fills the switch of a new service, because upstream models change and the
// operator - or the image check of a connection test - has the last word.
type Preset struct {
	Name       string
	BaseURL    string
	Model      string
	ImageInput bool
}

// PresetCustom is the choice of an address the platform knows nothing about.
const PresetCustom = "custom"

// PresetList is the preset dictionary in the order the console offers it,
// ending with PresetCustom, which fills nothing.
var PresetList = []Preset{
	// DeepSeek's current model name; deepseek-v4-flash is a legacy alias of
	// the same V4.1 Flash model, which accepts images.
	{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", ImageInput: true},
	{Name: "bailian", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "qwen-plus"},
	{Name: "siliconflow", BaseURL: "https://api.siliconflow.cn/v1", Model: "deepseek-ai/DeepSeek-V3"},
	{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "deepseek/deepseek-chat"},
	{Name: "poe", BaseURL: "https://api.poe.com/v1", Model: "GPT-5", ImageInput: true},
	{Name: PresetCustom},
}

// Presets are the presets that fill something, by name.
var Presets = func() map[string]Preset {
	out := make(map[string]Preset, len(PresetList))
	for _, p := range PresetList {
		if p.Name != PresetCustom {
			out[p.Name] = p
		}
	}
	return out
}()
