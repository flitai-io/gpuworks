package deploy

import (
	"slices"
	"strings"
	"testing"
)

// A stored configuration can hold a value no request may state (a curated
// template wrote it, or the dictionary's bounds moved since). An edit that
// leaves it alone keeps it; an edit that changes another key is not refused
// for it; only what the edit itself states is measured.
func TestMaterializeBaseKeepsStoredValuesAndChecksOnlyTheEdit(t *testing.T) {
	base := LaunchConfig{MaxModelLen: 32768, GPUMemUtil: 0.99, MaxNumSeqs: 256}
	out, err := Materialize(MaterializeInput{
		Engine: EngineVLLM, Base: &base,
		Raw: map[string]any{"max_model_len": 16384, "gpu_memory_utilization": 0.99, "max_num_seqs": 256},
	})
	if err != nil {
		t.Fatalf("an edit restating an out-of-bounds stored value was refused: %v", err)
	}
	if out.Quick.MaxModelLen != 16384 || out.Quick.GPUMemUtil != 0.99 || out.Quick.MaxNumSeqs != 256 {
		t.Fatalf("edited config = %+v", out.Quick)
	}

	if _, err := Materialize(MaterializeInput{
		Engine: EngineVLLM, Base: &base,
		Raw: map[string]any{"gpu_memory_utilization": 0.995},
	}); err == nil || !strings.Contains(err.Error(), "gpu_memory_utilization") {
		t.Fatalf("an edited value past the bounds = %v, want refused", err)
	}
}

// An edit adds no dictionary default the stored row did not have, keeps the
// stored keys the request does not mention, and drops what it unsets.
func TestMaterializeBaseAddsNoDefaultsAndHonoursUnset(t *testing.T) {
	base := LaunchConfig{MaxModelLen: 8192, KVOffloadingSize: 16}
	out, err := Materialize(MaterializeInput{
		Engine: EngineVLLM, Base: &base,
		Unset: []string{"kv_offloading_size"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.MaxModelLen != 8192 {
		t.Fatalf("unmentioned stored key lost: %+v", out.Quick)
	}
	if out.Quick.KVOffloadingSize != 0 {
		t.Fatalf("unset stored key survived: %v", out.Quick.KVOffloadingSize)
	}
	if out.Quick.GPUMemUtil != 0 {
		t.Fatalf("an edit gained the dictionary default gpu_memory_utilization=%v", out.Quick.GPUMemUtil)
	}

	fresh, err := Materialize(MaterializeInput{Engine: EngineVLLM})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Quick.GPUMemUtil == 0 {
		t.Fatal("a new request lost its dictionary defaults")
	}
}

// The stored configuration ranks above the template's arguments and below a
// flag typed into extra_args, which moves into its field as on create.
func TestMaterializeBasePrecedence(t *testing.T) {
	base := LaunchConfig{MaxModelLen: 8192, MaxNumSeqs: 64}
	out, err := Materialize(MaterializeInput{
		Engine: EngineVLLM, Base: &base,
		TemplateArgs: []string{"--max-model-len", "4096"},
		ExtraArgs:    []string{"--max-num-seqs", "128"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.MaxModelLen != 8192 {
		t.Fatalf("template argument beat the stored value: %d", out.Quick.MaxModelLen)
	}
	if out.Quick.MaxNumSeqs != 128 {
		t.Fatalf("extra_args flag lost to the stored value: %d", out.Quick.MaxNumSeqs)
	}
}

func TestMaterializeEditRestatedEagerOffAllowsNewExtraArgs(t *testing.T) {
	base := LaunchConfig{EnforceEager: boolp(false)}
	out, err := Materialize(MaterializeInput{
		Engine: EngineVLLM, Base: &base,
		Raw:       map[string]any{"enforce_eager": false},
		ExtraArgs: []string{"--enforce-eager"},
	})
	if err != nil {
		t.Fatalf("unchanged draft Off conflicted with newly typed On: %v", err)
	}
	if out.Quick.EnforceEager == nil || !*out.Quick.EnforceEager || len(out.Moved) != 1 || out.Moved[0].Key != "enforce_eager" {
		t.Fatalf("new extra_args did not replace stored Off: %+v", out)
	}
	// A field the edit actually changes remains explicit, and a different
	// extra_args value must still be rejected rather than silently winning.
	base.EnforceEager = boolp(true)
	if _, err := Materialize(MaterializeInput{
		Engine: EngineVLLM, Base: &base,
		Raw:       map[string]any{"enforce_eager": false},
		ExtraArgs: []string{"--enforce-eager"},
	}); err == nil || !strings.Contains(err.Error(), "enforce_eager") {
		t.Fatalf("changed Off and extra_args On = %v, want conflict", err)
	}
}

func TestEditedParams(t *testing.T) {
	base := LaunchConfig{MaxModelLen: 8192, GPUMemUtil: 0.9, KVOffloadingSize: 8}
	edited, removed, err := EditedParams(EngineVLLM, base, map[string]any{
		"max_model_len": 8192.0, "gpu_memory_utilization": "0.85", "max_num_seqs": 32, "dtype": nil,
	}, []string{"kv_offloading_size", "kv_offloading_size", "max_num_batched_tokens"})
	if err != nil {
		t.Fatal(err)
	}
	if _, restated := edited["max_model_len"]; restated {
		t.Fatalf("a restated stored value counted as edited: %v", edited)
	}
	if len(edited) != 2 || edited["gpu_memory_utilization"] != "0.85" || edited["max_num_seqs"] != 32 {
		t.Fatalf("edited = %v", edited)
	}
	if !slices.Equal(removed, []string{"kv_offloading_size"}) {
		t.Fatalf("removed = %v, want only the stored key, once", removed)
	}
	if _, _, err := EditedParams("no-such-engine", base, nil, nil); err == nil {
		t.Fatal("unknown engine accepted")
	}
}
