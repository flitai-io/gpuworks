package deploy

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// profileGoldenDir pins the engine profiles GET /api/v1/engines serves (the
// parameter dictionary rides in them as param_specs), per engine and per
// target vendor, byte for byte. The KV offload backend seam may not change a
// profile while it registers only the native backend.
//
//	GPUWORKS_UPDATE_PROFILE_GOLDEN=1 go test ./internal/server/deploy -run TestEngineProfileGoldens
const profileGoldenDir = "testdata/profile"

const profileGoldenUpdateEnv = "GPUWORKS_UPDATE_PROFILE_GOLDEN"

func TestEngineProfileGoldens(t *testing.T) {
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		// The empty vendor is the request without target_vendor.
		for _, vendor := range append([]string{""}, accelerator.KnownVendors()...) {
			name := engine + "_" + vendor
			if vendor == "" {
				name = engine + "_any"
			}
			t.Run(name, func(t *testing.T) {
				profile, err := ProfileForTarget(engine, vendor)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.MarshalIndent(profile, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				compareGolden(t, filepath.Join(profileGoldenDir, name+".golden"), append(encoded, '\n'), profileGoldenUpdateEnv)
			})
		}
	}
}
