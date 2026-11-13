package nodelabel

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The platform's own namespace is closed to written labels, in any letter
// case and at any subdomain depth. Everything outside it stays writable.
func TestWrittenLabelsRefuseTheReservedNamespace(t *testing.T) {
	for _, key := range []string{
		"gpuworks.ai/node",
		"gpuworks.ai/anything",
		"GPUWORKS.AI/node",
		"rack.gpuworks.ai/row",
		"a.b.gpuworks.ai/x",
	} {
		if err := ValidateWritten(map[string]string{key: "v"}); err == nil {
			t.Errorf("%q accepted", key)
		}
	}
	for _, key := range []string{
		"gpuworks.ai", // no path, so no domain at all
		"gpuworks-ai/node",
		"notgpuworks.ai/x",
		"gpuworks.ai.example.com/x",
		"example.com/gpuworks.ai",
		"pool",
	} {
		if err := ValidateWritten(map[string]string{key: "v"}); err != nil {
			t.Errorf("%q refused: %v", key, err)
		}
	}
}

func TestWrittenLabelGrammar(t *testing.T) {
	if err := ValidateWritten(map[string]string{"pool": ""}); err != nil {
		t.Fatalf("an empty value is a value: %v", err)
	}
	for name, labels := range map[string]map[string]string{
		"key starts with a dash":   {"-pool": "a"},
		"key has a space":          {"my pool": "a"},
		"key too long":             {strings.Repeat("k", 129): "a"},
		"value too long":           {"pool": strings.Repeat("v", 257)},
		"value has a control rune": {"pool": "a\nb"},
		"too many keys":            manyLabels(MaxKeys + 1),
	} {
		if err := ValidateWritten(labels); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateWritten(manyLabels(MaxKeys)); err != nil {
		t.Fatalf("%d keys refused: %v", MaxKeys, err)
	}
}

// Every system key is the platform's: a written label may never use it, and
// a requirement may - which is the whole point of having two validators.
func TestSystemKeysAreRequirableButNeverWritable(t *testing.T) {
	valid := map[string]string{
		KeyNode: "5c1e-uuid", KeyVendor: "nvidia", KeyCPUArch: "arm64",
		KeyGPUModel: "GeForce RTX 4090", KeyGPUArch: "ada",
		KeyComputeNetwork: "12", KeyStorageSyncNetwork: ManagementNetwork, KeyServingNetwork: "3",
		KeyStorage: "2", KeyProvider: "runpod", KeyRegion: "US-VA-1",
		KeyKubernetesCluster: "7c1b6f0e-uuid", KeyKubernetesNode: "worker-3",
	}
	if len(valid) != len(SystemKeys()) {
		t.Fatalf("test covers %d keys, SystemKeys has %d", len(valid), len(SystemKeys()))
	}
	for _, key := range SystemKeys() {
		if !strings.HasPrefix(key, Prefix) || !Reserved(key) || !IsSystemKey(key) {
			t.Errorf("%q is not a reserved system key", key)
		}
		if err := ValidateWritten(map[string]string{key: valid[key]}); err == nil {
			t.Errorf("%q writable", key)
		}
		got, err := NormalizeRequirements(Requirements{key: {valid[key]}})
		if err != nil {
			t.Errorf("%q=%q refused as a requirement: %v", key, valid[key], err)
			continue
		}
		if !reflect.DeepEqual(got, Requirements{key: {valid[key]}}) {
			t.Errorf("%q normalized to %v", key, got)
		}
	}
}

// The shape a deployment stores: system and written keys side by side, each
// key's values deduplicated and sorted, the caller's map left as it was.
func TestNormalizeRequirementsDedupesAndSorts(t *testing.T) {
	in := Requirements{
		KeyNode:   {"n-b", "n-a", "n-b"},
		KeyVendor: {"ascend", "ascend"},
		"pool":    {"inference", ""},
	}
	got, err := NormalizeRequirements(in)
	if err != nil {
		t.Fatal(err)
	}
	want := Requirements{
		KeyNode:   {"n-a", "n-b"},
		KeyVendor: {"ascend"},
		"pool":    {"", "inference"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(in[KeyNode], []string{"n-b", "n-a", "n-b"}) {
		t.Fatalf("input modified: %v", in[KeyNode])
	}
	if got, err := NormalizeRequirements(nil); err != nil || len(got) != 0 {
		t.Fatalf("no requirements = %v, %v", got, err)
	}
}

func TestNormalizeRequirementsRefuses(t *testing.T) {
	manyValues := make([]string, MaxValuesPerKey+1)
	for i := range manyValues {
		manyValues[i] = fmt.Sprintf("n-%d", i)
	}
	tooManyKeys := Requirements{}
	for key := range manyLabels(MaxKeys + 1) {
		tooManyKeys[key] = []string{"v"}
	}
	for name, c := range map[string]struct {
		in   Requirements
		want string
	}{
		// A misspelt system key would otherwise match nothing, silently.
		"misspelt system key":          {Requirements{Prefix + "gpu_model": {"A100"}}, "not a system label"},
		"system key in another case":   {Requirements{"GPUWORKS.AI/node": {"n"}}, "not a system label"},
		"reserved subdomain":           {Requirements{"rack.gpuworks.ai/row": {"7"}}, "not a system label"},
		"bad key grammar":              {Requirements{"my pool": {"a"}}, "invalid node label key"},
		"no value":                     {Requirements{"pool": {}}, "names no value"},
		"too many values":              {Requirements{KeyNode: manyValues}, "more than"},
		"too many keys":                {tooManyKeys, "at most"},
		"empty system value":           {Requirements{KeyGPUModel: {""}}, "empty value"},
		"control rune in value":        {Requirements{"pool": {"a\tb"}}, "invalid value"},
		"two vendors":                  {Requirements{KeyVendor: {"nvidia", "ascend"}}, "exactly one vendor"},
		"unknown vendor":               {Requirements{KeyVendor: {"intel"}}, "unknown vendor"},
		"vendor not in canonical form": {Requirements{KeyVendor: {"NVIDIA"}}, "unknown vendor"},
		"network id with a zero":       {Requirements{KeyComputeNetwork: {"012"}}, "neither a network id"},
		"network id zero":              {Requirements{KeyServingNetwork: {"0"}}, "neither a network id"},
		"network name":                 {Requirements{KeyStorageSyncNetwork: {"rdma-a"}}, "neither a network id"},
	} {
		got, err := NormalizeRequirements(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, %v; want an error containing %q", name, got, err, c.want)
		}
	}
	full := make([]string, MaxValuesPerKey)
	copy(full, manyValues)
	if _, err := NormalizeRequirements(Requirements{KeyNode: full}); err != nil {
		t.Fatalf("%d nodes refused: %v", MaxValuesPerKey, err)
	}
}

func manyLabels(n int) map[string]string {
	labels := make(map[string]string, n)
	for i := 0; i < n; i++ {
		labels[fmt.Sprintf("k%d", i)] = "v"
	}
	return labels
}
