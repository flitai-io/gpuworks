// Package modelconfig parses a model repository's config.json into a normalized
// struct used for KV-cache estimation. The file layout is HuggingFace's, which
// both weight providers publish unchanged; nothing here is specific to one
// provider or to the HuggingFace hub.
//
// Repositories spell the same architectural facts differently: some nest the
// language model under text_config / llm_config / talker_config, some call the
// layer count num_layers, and encoder-decoder models keep a separate decoder
// stack next to the encoder. Parse resolves those spellings into one shape so
// an estimator sees the same config whatever a repository happens to write.
// A field that is missing everywhere stays zero, and a value with an
// unexpected type (a vision tower whose image_size is a list, a layer count
// sent as a string) is ignored rather than failing the whole parse: a partial
// config still yields an estimate, an unparsed one yields none.
package modelconfig

import (
	"encoding/json"
	"io"
	"strings"
)

// Config is the normalized subset needed for estimation. Fields not present in a
// given model are left zero. The yaml tags let the inference catalog carry the
// same shape as a model-level declaration; they are all omitempty, so a
// declaration written back out lists only the facts it states.
type Config struct {
	Architectures         []string       `json:"architectures" yaml:"architectures,omitempty"`
	ModelType             string         `json:"model_type" yaml:"model_type,omitempty"`
	NumHiddenLayers       int            `json:"num_hidden_layers" yaml:"num_hidden_layers,omitempty"`
	HiddenSize            int            `json:"hidden_size" yaml:"hidden_size,omitempty"`
	NumAttentionHeads     int            `json:"num_attention_heads" yaml:"num_attention_heads,omitempty"`
	NumKeyValueHeads      int            `json:"num_key_value_heads" yaml:"num_key_value_heads,omitempty"` // GQA/MQA; defaults to NumAttentionHeads if absent
	HeadDim               int            `json:"head_dim" yaml:"head_dim,omitempty"`                       // if absent, derive HiddenSize/NumAttentionHeads
	MaxPositionEmbeddings int            `json:"max_position_embeddings" yaml:"max_position_embeddings,omitempty"`
	VocabSize             int            `json:"vocab_size" yaml:"vocab_size,omitempty"`
	TorchDtype            string         `json:"torch_dtype" yaml:"torch_dtype,omitempty"`
	RopeScaling           map[string]any `json:"rope_scaling" yaml:"rope_scaling,omitempty"`
	SlidingWindow         int            `json:"sliding_window" yaml:"sliding_window,omitempty"`
	// UseSlidingWindow is the config's own statement about whether
	// SlidingWindow is in force. It is a pointer because the Qwen2/Qwen2.5
	// family publishes a window value it explicitly disables, while other
	// families leave the key out entirely and mean the window is used.
	UseSlidingWindow *bool `json:"use_sliding_window,omitempty" yaml:"use_sliding_window,omitempty"`
	// MLA (DeepSeek) fields:
	KVLoraRank    int `json:"kv_lora_rank" yaml:"kv_lora_rank,omitempty"`
	QKRopeHeadDim int `json:"qk_rope_head_dim" yaml:"qk_rope_head_dim,omitempty"`
	QKNopeHeadDim int `json:"qk_nope_head_dim" yaml:"qk_nope_head_dim,omitempty"`
	VHeadDim      int `json:"v_head_dim" yaml:"v_head_dim,omitempty"`
	// Layer typing for hybrid/SWA models:
	LayerTypes []string `json:"layer_types" yaml:"layer_types,omitempty"`
	// KVCache is deliberately not a config.json field. config.json's use_cache
	// cannot carry this meaning: Mistral's native params.json sets use_cache
	// false for training-time reasons on models that are served with a KV cache,
	// and diffusion repositories publish no config.json at all. Only the
	// inference catalog sets it, to state that a model has no token-indexed KV
	// pool to size. nil keeps the ordinary assumption that caching is available.
	KVCache *bool `json:"kv_cache,omitempty" yaml:"kv_cache,omitempty"`
	// Nested config used by multimodal / some text models:
	TextConfig    *Config `json:"text_config" yaml:"text_config,omitempty"`
	ThinkerConfig *Config `json:"thinker_config" yaml:"thinker_config,omitempty"`
	// Multimodal towers are preserved separately. Text fields are still promoted
	// from TextConfig, but callers can now size encoder runtime caches and avoid
	// treating every parseable multimodal config as a plain text model.
	VisionConfig *TowerConfig `json:"vision_config" yaml:"vision_config,omitempty"`
	AudioConfig  *TowerConfig `json:"audio_config" yaml:"audio_config,omitempty"`
}

// TowerConfig captures the cross-family dimensions used to estimate vision and
// audio encoder output caches. Unknown vendor-specific fields remain in the raw
// model metadata stored by the server.
type TowerConfig struct {
	ModelType             string `json:"model_type" yaml:"model_type,omitempty"`
	HiddenSize            int    `json:"hidden_size" yaml:"hidden_size,omitempty"`
	DModel                int    `json:"d_model" yaml:"d_model,omitempty"`
	IntermediateSize      int    `json:"intermediate_size" yaml:"intermediate_size,omitempty"`
	NumHiddenLayers       int    `json:"num_hidden_layers" yaml:"num_hidden_layers,omitempty"`
	EncoderLayers         int    `json:"encoder_layers" yaml:"encoder_layers,omitempty"`
	NumAttentionHeads     int    `json:"num_attention_heads" yaml:"num_attention_heads,omitempty"`
	EncoderAttentionHeads int    `json:"encoder_attention_heads" yaml:"encoder_attention_heads,omitempty"`
	ImageSize             int    `json:"image_size" yaml:"image_size,omitempty"`
	PatchSize             int    `json:"patch_size" yaml:"patch_size,omitempty"`
	NumChannels           int    `json:"num_channels" yaml:"num_channels,omitempty"`
	MaxPositionEmbeddings int    `json:"max_position_embeddings" yaml:"max_position_embeddings,omitempty"`
	MaxSourcePositions    int    `json:"max_source_positions" yaml:"max_source_positions,omitempty"`
	NumMelBins            int    `json:"num_mel_bins" yaml:"num_mel_bins,omitempty"`
	ProjectionDim         int    `json:"projection_dim" yaml:"projection_dim,omitempty"`
	OutputDim             int    `json:"output_dim" yaml:"output_dim,omitempty"`
	OutHiddenSize         int    `json:"out_hidden_size" yaml:"out_hidden_size,omitempty"`
	EmbedDim              int    `json:"embed_dim" yaml:"embed_dim,omitempty"`
	Depth                 int    `json:"depth" yaml:"depth,omitempty"`
	SpatialMergeSize      int    `json:"spatial_merge_size" yaml:"spatial_merge_size,omitempty"`
}

// Parse decodes config.json. If a nested language-model config is present
// (text_config, thinker_config, llm_config, …), the top-level fields win and the
// nested ones fill whatever the top level left unset.
func Parse(r io.Reader) (*Config, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		// Python's json.dump writes Infinity and NaN by default, and Go's
		// decoder rejects them, so an otherwise ordinary config would be lost to
		// one non-finite value (NVIDIA's Nemotron-Nano-9B-v2-Base is one).
		// Retry once with those literals neutralised.
		sanitized, changed := replaceNonFinite(body)
		if !changed {
			return nil, err
		}
		if retryErr := json.Unmarshal(sanitized, &raw); retryErr != nil {
			return nil, err
		}
	}
	return FromMap(raw), nil
}

// EffectiveKVHeads returns NumKeyValueHeads, falling back to NumAttentionHeads.
func (c *Config) EffectiveKVHeads() int {
	if c.NumKeyValueHeads > 0 {
		return c.NumKeyValueHeads
	}
	return c.NumAttentionHeads
}

// EffectiveHeadDim returns HeadDim, or HiddenSize/NumAttentionHeads if HeadDim==0.
func (c *Config) EffectiveHeadDim() int {
	if c.HeadDim > 0 {
		return c.HeadDim
	}
	if c.NumAttentionHeads == 0 {
		return 0
	}
	return c.HiddenSize / c.NumAttentionHeads
}

// IsMLA reports whether this is a DeepSeek-style MLA config (KVLoraRank>0).
func (c *Config) IsMLA() bool {
	return c.KVLoraRank > 0
}

// HasMultimodalTower reports whether config.json declares a vision or audio
// encoder alongside the language model.
func (c *Config) HasMultimodalTower() bool {
	return c != nil && (c.VisionConfig != nil || c.AudioConfig != nil)
}

// HasRecurrentLayers reports whether any layer keeps a recurrent state instead
// of a token-indexed KV cache: linear attention (gated delta net, KDA), Mamba,
// short convolutions. Such hybrids need the engine's hybrid KV cache manager
// to give those layers their own block groups.
func (c *Config) HasRecurrentLayers() bool {
	if c == nil {
		return false
	}
	for _, kind := range c.LayerTypes {
		kind = strings.ToLower(kind)
		for _, marker := range recurrentLayerMarkers {
			if strings.Contains(kind, marker) {
				return true
			}
		}
	}
	return false
}

// recurrentLayerMarkers are the substrings that name a recurrent layer kind in
// normalized layer_types (layerTypes): linear_attention, mamba/mamba2,
// short_conv, and the gated-delta spellings some configs use.
var recurrentLayerMarkers = []string{"linear", "mamba", "conv", "delta", "kda", "ssm"}

// CachesKVTokens reports whether the model keeps a token-indexed KV cache. A
// config that declares kv_cache: false (the inference catalog's statement for
// diffusion and encoder-only pipelines) has no pool to size; every other config
// is assumed to cache, which is the ordinary serving behaviour.
func (c *Config) CachesKVTokens() bool {
	return c == nil || c.KVCache == nil || *c.KVCache
}
