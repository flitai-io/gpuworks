package deploy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A mount row may name a registered model instead of a host directory. On
// the wire it is model_id, which the deploy form and API clients send.
func TestContainerMountModelRowWire(t *testing.T) {
	overrides, err := DecodeContainerOverrides([]byte(`{"extra_mounts":[{"model_id":7,"container_path":"/draft","purpose":"models"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := overrides.ExtraMounts[0]; got.ModelID != 7 || got.ContainerPath != "/draft" || got.HostPath != "" {
		t.Fatalf("decoded %+v", got)
	}
	encoded, err := json.Marshal(overrides.ExtraMounts[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"model_id":7,"container_path":"/draft","purpose":"models"}` {
		t.Fatalf("wire %s", encoded)
	}
}

// A row states exactly one source: a model row that also names a host path or
// a volume is refused, and a row with none of the three still is.
func TestContainerMountModelRowValidation(t *testing.T) {
	for _, tc := range []struct {
		mount ContainerMount
		ok    bool
		want  string
	}{
		{ContainerMount{ModelID: 7, ContainerPath: "/draft"}, true, ""},
		{ContainerMount{ModelID: 7, HostPath: "/srv/draft", ContainerPath: "/draft"}, false, "states a model and"},
		{ContainerMount{ModelID: 7, Volume: "drafts", ContainerPath: "/draft"}, false, "states a model and"},
		{ContainerMount{ModelID: 7, ContainerPath: "draft"}, false, "must be absolute"},
		{ContainerMount{ContainerPath: "/draft"}, false, "needs host_path, volume or model_id"},
	} {
		err := ContainerOverrides{ExtraMounts: []ContainerMount{tc.mount}}.Validate()
		if tc.ok != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%+v: err=%v, want ok=%v %q", tc.mount, err, tc.ok, tc.want)
		}
	}
}

// The launch mounts the directory the node has for the model, read-only even
// when the row did not ask; a preview with no path for it shows the
// placeholder instead of an empty source.
func TestResolveLaunchMountsTheModelRowsNodePath(t *testing.T) {
	overrides := ContainerOverrides{ExtraMounts: []ContainerMount{
		{ModelID: 7, ContainerPath: "/draft"},
		{HostPath: "/srv/data", ContainerPath: "/data"},
	}}
	input := ResolveLaunchInput{
		Engine:             vLLMEngine{},
		Params:             Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1},
		ContainerOverrides: overrides,
	}
	find := func(mounts []proto.MountSpec, target string) proto.MountSpec {
		index := slices.IndexFunc(mounts, func(mount proto.MountSpec) bool { return mount.Target == target })
		if index < 0 {
			t.Fatalf("no mount at %s in %+v", target, mounts)
		}
		return mounts[index]
	}

	preview, err := ResolveLaunch(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := find(preview.Mounts, "/draft"); got.Source != "$MODEL_7_PATH" || !got.ReadOnly || got.Purpose != "models" {
		t.Fatalf("preview model mount %+v", got)
	}

	input.ModelPaths = map[uint]string{7: "/var/lib/gpuworks/models/7"}
	launch, err := ResolveLaunch(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := find(launch.Mounts, "/draft"); got.Source != "/var/lib/gpuworks/models/7" || !got.ReadOnly || got.Volume != "" {
		t.Fatalf("launch model mount %+v", got)
	}
	if got := find(launch.Mounts, "/data"); got.Source != "/srv/data" || got.ReadOnly {
		t.Fatalf("host path mount changed: %+v", got)
	}
}

func TestModelMountIDsListsEachModelOnce(t *testing.T) {
	overrides := ContainerOverrides{ExtraMounts: []ContainerMount{
		{ModelID: 9, ContainerPath: "/a"}, {HostPath: "/srv", ContainerPath: "/b"},
		{ModelID: 7, ContainerPath: "/c"}, {ModelID: 9, ContainerPath: "/d"},
	}}
	if got := overrides.ModelMountIDs(); !slices.Equal(got, []uint{9, 7}) {
		t.Fatalf("ModelMountIDs = %v", got)
	}
	if got := (ContainerOverrides{}).ModelMountIDs(); got != nil {
		t.Fatalf("empty overrides = %v", got)
	}
}
