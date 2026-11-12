package modelconfig

import (
	"strings"
	"testing"
)

// The fixtures below are trimmed copies of real repository config.json files,
// one per shape that used to be unreadable. They are the regression net for
// "the estimator returned nothing for a model that is in the catalog".
func parse(t *testing.T, raw string) *Config {
	t.Helper()
	cfg, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func TestParseResolvesLayerAndHeadSpellings(t *testing.T) {
	// EXAONE spells the layer count num_layers.
	cfg := parse(t, `{"model_type":"exaone","num_layers":32,"hidden_size":4096,
		"num_attention_heads":32,"num_key_value_heads":8,"head_dim":128}`)
	if cfg.NumHiddenLayers != 32 || cfg.NumAttentionHeads != 32 || cfg.NumKeyValueHeads != 8 {
		t.Fatalf("EXAONE shape unresolved: %+v", cfg)
	}

	// A quoted number must still count as a number.
	quoted := parse(t, `{"model_type":"m","num_hidden_layers":"28","num_attention_heads":"28","hidden_size":"3584"}`)
	if quoted.NumHiddenLayers != 28 || quoted.EffectiveHeadDim() != 128 {
		t.Fatalf("quoted numbers unresolved: %+v", quoted)
	}
}

func TestParsePromotesEveryEstimationField(t *testing.T) {
	// Kimi-K2.5 keeps its MLA ranks inside text_config. Promoting only the
	// ordinary shape fields made the estimator read an MLA model as a plain
	// multi-head one, overstating bytes per token by more than 20x.
	cfg := parse(t, `{"model_type":"kimi_k25","dtype":"bfloat16","text_config":{
		"model_type":"kimi_k2","num_hidden_layers":61,"hidden_size":7168,
		"num_attention_heads":64,"num_key_value_heads":64,
		"kv_lora_rank":512,"qk_rope_head_dim":64,"qk_nope_head_dim":128,"v_head_dim":128,
		"max_position_embeddings":262144}}`)
	if !cfg.IsMLA() || cfg.KVLoraRank != 512 || cfg.QKRopeHeadDim != 64 {
		t.Fatalf("MLA ranks not promoted: %+v", cfg)
	}
	if cfg.NumHiddenLayers != 61 || cfg.MaxPositionEmbeddings != 262144 {
		t.Fatalf("nested shape not promoted: %+v", cfg)
	}
}

func TestParsePromotesVendorNestedLanguageConfig(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"llm_config", `{"model_type":"ovis2_5","hidden_size":2048,"llm_config":{
			"num_hidden_layers":28,"hidden_size":2048,"num_attention_heads":16,"num_key_value_heads":2,"head_dim":128}}`},
		{"language_config", `{"model_type":"moss_tts_delay","language_config":{
			"num_hidden_layers":28,"hidden_size":1024,"num_attention_heads":16,"num_key_value_heads":8,
			"head_dim":64,"layer_types":["full_attention","linear_attention"]}}`},
		{"talker_config", `{"model_type":"qwen3_tts","talker_config":{
			"num_hidden_layers":28,"hidden_size":1024,"num_attention_heads":16,"num_key_value_heads":8,"head_dim":128}}`},
		{"language_model", `{"model_type":"deepseek_vl_v2","language_config":{
			"num_hidden_layers":30,"hidden_size":4096,"num_attention_heads":32,"num_key_value_heads":32,"head_dim":128}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := parse(t, test.json)
			if cfg.NumHiddenLayers == 0 || cfg.NumAttentionHeads == 0 {
				t.Fatalf("%s not promoted: %+v", test.name, cfg)
			}
			if cfg.EffectiveHeadDim() == 0 {
				t.Fatalf("%s head dim unresolved: %+v", test.name, cfg)
			}
		})
	}
}

func TestParseEncoderDecoderUsesDecoderStack(t *testing.T) {
	// Whisper counts encoder layers in num_hidden_layers and names only decoder
	// heads; the decoder is the stack that keeps a KV cache. Whisper-large-v3
	// turbo is the case that makes the difference visible.
	cfg := parse(t, `{"model_type":"whisper","num_hidden_layers":32,"encoder_layers":32,
		"encoder_attention_heads":20,"decoder_layers":4,"decoder_attention_heads":20,
		"d_model":1280,"max_target_positions":448}`)
	if cfg.NumHiddenLayers != 4 || cfg.NumAttentionHeads != 20 {
		t.Fatalf("decoder stack not selected: %+v", cfg)
	}
	if cfg.EffectiveHeadDim() != 64 {
		t.Fatalf("decoder head dim = %d, want 1280/20", cfg.EffectiveHeadDim())
	}
}

func TestParseHybridLayerTyping(t *testing.T) {
	// Nemotron-H lists one entry per layer, including feed-forward-only layers.
	cfg := parse(t, `{"model_type":"nemotron_h","layers_block_type":["mamba","moe","attention","moe"],
		"num_attention_heads":64,"num_key_value_heads":2,"head_dim":128,"hidden_size":8192}`)
	if len(cfg.LayerTypes) != 4 {
		t.Fatalf("layers_block_type not read: %+v", cfg.LayerTypes)
	}
	// Jamba and Nemotron encode the same list as a character pattern where one
	// character is one layer: 'M' Mamba, '*' attention, '-' feed-forward. Reading
	// '-' as a separator shrinks the list below the real layer count.
	pattern := parse(t, `{"model_type":"jamba","hybrid_override_pattern":"M-M-*-M","num_hidden_layers":7,
		"num_attention_heads":32,"hidden_size":4096}`)
	want := []string{"mamba", "mlp", "mamba", "mlp", "full_attention", "mlp", "mamba"}
	if len(pattern.LayerTypes) != len(want) {
		t.Fatalf("hybrid pattern not expanded: %+v", pattern.LayerTypes)
	}
	for index := range want {
		if pattern.LayerTypes[index] != want[index] {
			t.Fatalf("layer %d = %q, want %q (%+v)", index, pattern.LayerTypes[index], want[index], pattern.LayerTypes)
		}
	}
}

func TestParseLinearAttentionLayerIndexes(t *testing.T) {
	// Kimi-Linear publishes which layers keep a full attention cache and which
	// keep a recurrent state. Without it the estimator sizes every layer as if it
	// cached K/V, which overstates the pool several times over.
	cfg := parse(t, `{"model_type":"kimi_linear","num_hidden_layers":8,"hidden_size":2304,
		"num_attention_heads":32,"num_key_value_heads":32,"kv_lora_rank":512,"qk_rope_head_dim":64,
		"linear_attn_config":{"full_attn_layers":[4,8],"kda_layers":[1,2,3,5,6,7]}}`)
	if len(cfg.LayerTypes) != 8 {
		t.Fatalf("linear_attn_config not read: %+v", cfg.LayerTypes)
	}
	attention := 0
	for _, layerType := range cfg.LayerTypes {
		if layerType == "full_attention" {
			attention++
		}
	}
	if attention != 2 {
		t.Fatalf("attention layers = %d, want 2 (%+v)", attention, cfg.LayerTypes)
	}
}

func TestParseToleratesMalformedTower(t *testing.T) {
	// Intern-S1 publishes image_size as a list, which used to fail the whole
	// decode and left the model with no estimate at all.
	cfg := parse(t, `{"model_type":"internvl_chat","vision_config":{"image_size":[448,448],"patch_size":14},
		"llm_config":{"num_hidden_layers":28,"hidden_size":3584,"num_attention_heads":28,"num_key_value_heads":4}}`)
	if cfg.NumHiddenLayers != 28 || cfg.EffectiveHeadDim() != 128 {
		t.Fatalf("text shape lost to a malformed tower: %+v", cfg)
	}
	if cfg.VisionConfig == nil || cfg.VisionConfig.ImageSize != 0 || cfg.VisionConfig.PatchSize != 14 {
		t.Fatalf("tower not decoded tolerantly: %+v", cfg.VisionConfig)
	}
}

func TestParseChoosesOneStack(t *testing.T) {
	// A complete stack at the top level is the model; the nested level is
	// another stack and must not be mixed into it.
	complete := parse(t, `{"model_type":"wrapper","num_hidden_layers":80,"hidden_size":8192,
		"num_attention_heads":64,"head_dim":128,
		"text_config":{"num_hidden_layers":24,"hidden_size":2048,"num_attention_heads":16}}`)
	if complete.NumHiddenLayers != 80 || complete.NumAttentionHeads != 64 {
		t.Fatalf("top level must describe the model: %+v", complete)
	}

	// A level that says only part of the stack is completed by a level that
	// declares the same depth (a repository may split one stack across keys).
	split := parse(t, `{"model_type":"wrapper","num_hidden_layers":24,
		"text_config":{"num_hidden_layers":24,"hidden_size":2048,"num_attention_heads":16}}`)
	if split.NumHiddenLayers != 24 || split.NumAttentionHeads != 16 || split.EffectiveHeadDim() != 128 {
		t.Fatalf("split stack not completed: %+v", split)
	}

	// A level with another depth is another stack. Taking its head size would
	// describe a model that does not exist: Qwen2.5-Omni's talker is 24 layers
	// of width 896 next to a 36-layer thinker, and mixing them halved the KV
	// footprint of the thinker.
	second := parse(t, `{"model_type":"omni","hidden_size":2048,"num_hidden_layers":36,
		"thinker_config":{"text_config":{"num_hidden_layers":36,"hidden_size":2048,"num_attention_heads":16,"num_key_value_heads":2}},
		"talker_config":{"num_hidden_layers":24,"hidden_size":896,"num_attention_heads":14,"num_key_value_heads":2,"head_dim":64}}`)
	if second.NumHiddenLayers != 36 || second.NumKeyValueHeads != 2 {
		t.Fatalf("thinker stack not selected: %+v", second)
	}
	if second.EffectiveHeadDim() != 128 {
		t.Fatalf("head dim = %d, want the thinker's 2048/16 (the talker's 64 must not leak in)", second.EffectiveHeadDim())
	}
}

func TestParseReportsUnreadableConfig(t *testing.T) {
	if _, err := Parse(strings.NewReader(`{"unterminated":`)); err == nil {
		t.Fatal("malformed JSON must stay an error so callers can tell partial from unusable")
	}
}

func TestCachesKVTokens(t *testing.T) {
	cases := []struct {
		json string
		want bool
	}{
		{`{"model_type":"qwen3"}`, true},
		{`{"model_type":"sd3","kv_cache":false}`, false},
		// config.json's own use_cache is a training-time flag on some
		// repositories (Mistral's params.json sets it false on models that are
		// served with a KV cache), so it must not be read as "no cache".
		{`{"model_type":"voxtral_tts","use_cache":false}`, true},
	}
	for _, test := range cases {
		cfg := parse(t, test.json)
		if got := cfg.CachesKVTokens(); got != test.want {
			t.Errorf("%s: CachesKVTokens = %v, want %v", test.json, got, test.want)
		}
	}
	var nilConfig *Config
	if !nilConfig.CachesKVTokens() {
		t.Error("an unknown model is assumed to cache")
	}
}

func TestParseAcceptsPythonNonFiniteLiterals(t *testing.T) {
	// json.dump writes Infinity/NaN by default, and Go's decoder refuses them.
	// One such value used to cost the whole config, and with it the estimate.
	cfg := parse(t, `{"model_type":"nemotron_h","num_hidden_layers":56,"hidden_size":4480,
		"num_attention_heads":40,"num_key_value_heads":8,"head_dim":128,"time_step_limit":[0.0,Infinity],
		"note":"Infinity inside a string stays put"}`)
	if cfg.NumHiddenLayers != 56 || cfg.NumKeyValueHeads != 8 {
		t.Fatalf("shape lost to a non-finite literal: %+v", cfg)
	}
	if _, err := Parse(strings.NewReader(`{"broken":`)); err == nil {
		t.Fatal("a document that is not JSON at all must still fail")
	}
	if _, err := Parse(strings.NewReader(`{"value": Inf}`)); err == nil {
		t.Fatal("a malformed non-finite spelling must still fail")
	}
}

func TestHasRecurrentLayers(t *testing.T) {
	tests := []struct {
		name string
		json string
		want bool
	}{
		// Qwen3.5/3.6/3.8 (Qwen3_5ForConditionalGeneration): gated delta net
		// layers under text_config, three to every full attention layer.
		{"qwen3_5 nested", `{"model_type":"qwen3_5","text_config":{"model_type":"qwen3_5_text",
			"num_hidden_layers":4,"hidden_size":1024,"num_attention_heads":8,"num_key_value_heads":2,"head_dim":256,
			"layer_types":["linear_attention","linear_attention","linear_attention","full_attention"]}}`, true},
		{"jamba pattern", `{"model_type":"jamba","hybrid_override_pattern":"M-*","num_hidden_layers":3,
			"num_attention_heads":32,"hidden_size":4096}`, true},
		{"kimi linear index list", `{"model_type":"kimi_linear","num_hidden_layers":4,"num_attention_heads":32,
			"hidden_size":2048,"linear_attn_config":{"full_attn_layers":[4]}}`, true},
		{"sliding window only", `{"model_type":"gemma3_text","num_hidden_layers":2,"num_attention_heads":8,
			"hidden_size":1024,"layer_types":["sliding_attention","full_attention"]}`, false},
		{"dense without layer_types", `{"model_type":"qwen3","num_hidden_layers":28,"num_attention_heads":16,
			"num_key_value_heads":8,"hidden_size":1024,"head_dim":128}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := parse(t, test.json)
			if got := cfg.HasRecurrentLayers(); got != test.want {
				t.Fatalf("HasRecurrentLayers() = %v, want %v (layer_types %v)", got, test.want, cfg.LayerTypes)
			}
		})
	}
	var missing *Config
	if missing.HasRecurrentLayers() {
		t.Fatal("a nil config has no recurrent layers")
	}
}
