package seed

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const strataRunnerSpecName = "iq2xs-strata-nvidia"
const strataRunnerImage = "quay.io/gpuworks/gpuworks-runner:cuda13.0-strata0.1.38-amd64-slot31-r5"

func TestStrataRunnerBuiltInContract(t *testing.T) {
	_, original := catalogSpec(t, strataModelID, "iq2xs-strata-nvidia-single-gpu")
	_, runner := catalogSpec(t, strataModelID, strataRunnerSpecName)
	if original.Image != "gpuworks/strata:0.1-gw6" || len(original.ImageSources) != 3 {
		t.Fatal("the published gw6 profile and its mirrors must remain available")
	}
	if original.DisplayNameZHCN != "Turing 及以上 · IQ2_XS · Strata gw6（旧镜像）" ||
		original.DisplayNameEN != "Turing+ · IQ2_XS · Strata gw6 (legacy image)" {
		t.Fatal("the gw6 display names must identify the legacy image")
	}
	if runner.Image != strataRunnerImage || !reflect.DeepEqual(runner.ImageSources, []string{
		"registry.zstack.ai/gpuworks/gpuworks-runner:cuda13.0-strata0.1.38-amd64-slot31-r5",
	}) {
		t.Fatal("all profiles must select the verified r5 image and its published mirrors")
	}
	if runner.DisplayNameZHCN != "Turing 及以上 · IQ2_XS · Strata r5" ||
		runner.DisplayNameEN != "Turing+ · IQ2_XS · Strata r5" {
		t.Fatal("both display names must identify the published Strata revision")
	}
	// The published image retains the optional PLE arguments. Nothing in this profile
	// silently opts into a host-specific cache path or changes the model,
	// tokenizer, API, queue asset, GPU assignment or memory admission contract.
	runner.Name = original.Name
	runner.DisplayNameZHCN, runner.DisplayNameEN = original.DisplayNameZHCN, original.DisplayNameEN
	runner.Image, runner.ImageSources = original.Image, original.ImageSources
	if !reflect.DeepEqual(runner, original) {
		t.Fatal("the runner changed a launch contract beyond its identity and image")
	}
}

// The same reconciliation called at server startup must create the four profiles on
// a fresh database and add them to an existing gw6 installation, without import.
func TestStrataRunnerSeedsWithoutImport(t *testing.T) {
	catalog, digest, err := CatalogWithDigest()
	if err != nil {
		t.Fatal(err)
	}
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "upgrade"
		}
		t.Run(name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			var oldID uint
			if upgrade {
				model, original := catalogSpec(t, strataModelID, "iq2xs-strata-nvidia-single-gpu")
				// Seed the published gw6 labels, before this catalog distinguishes
				// its legacy image from the four new runner profiles.
				original.DisplayNameZHCN = "Turing 及以上 · GGUF IQ2_XS"
				original.DisplayNameEN = "Turing+ · GGUF IQ2_XS"
				model.Specs = []inferencecatalog.InferenceSpec{original}
				previous := inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}}
				if err := inferencecatalog.ReconcileUpstreamCatalog(st, previous, "previous-catalog"); err != nil {
					t.Fatal(err)
				}
				row, err := st.InferenceModelByModelID(strataModelID)
				if err != nil || len(row.Specs) != 1 {
					t.Fatalf("prepare existing gw6 installation: %v", err)
				}
				oldID = row.Specs[0].ID
			}
			runnerIDs := map[string]uint{}
			for attempt := 0; attempt < 2; attempt++ {
				if err := inferencecatalog.ReconcileUpstreamCatalog(st, catalog, digest); err != nil {
					t.Fatal(err)
				}
				row, err := st.InferenceModelByModelID(strataModelID)
				if err != nil {
					t.Fatal(err)
				}
				if len(row.Specs) != 5 {
					t.Fatalf("startup exposed %d specs, want gw6 plus four new profiles", len(row.Specs))
				}
				found, foundGW6 := 0, false
				for _, spec := range row.Specs {
					switch {
					case spec.Name == "iq2xs-strata-nvidia-single-gpu":
						foundGW6 = true
						if oldID != 0 && spec.ID != oldID {
							t.Error("upgrade replaced the existing gw6 spec identity")
						}
						oldID = spec.ID
						if spec.Image != "gpuworks/strata:0.1-gw6" ||
							spec.DisplayNameZHCN != "Turing 及以上 · IQ2_XS · Strata gw6（旧镜像）" ||
							spec.DisplayNameEN != "Turing+ · IQ2_XS · Strata gw6 (legacy image)" {
							t.Error("startup must retain gw6 and mark it as the legacy image")
						}
					case isStrataRunnerSpec(spec.Name):
						found++
						if spec.Source != "upstream" || spec.Image != strataRunnerImage || spec.PackageDigest != digest {
							t.Error("the runner is not a built-in profile of the embedded package")
						}
						if runnerIDs[spec.Name] != 0 && spec.ID != runnerIDs[spec.Name] {
							t.Error("repeated startup replaced the runner spec identity")
						}
						var sources []string
						if err := json.Unmarshal([]byte(spec.ImageSources), &sources); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(sources, []string{
							"registry.zstack.ai/gpuworks/gpuworks-runner:cuda13.0-strata0.1.38-amd64-slot31-r5",
						}) {
							t.Error("startup must persist the published runner mirror")
						}
						runnerIDs[spec.Name] = spec.ID
					}
				}
				if found != 4 || !foundGW6 {
					t.Fatalf("startup exposed %d runner specs, gw6=%t; want four and gw6", found, foundGW6)
				}
			}
		})
	}
}
