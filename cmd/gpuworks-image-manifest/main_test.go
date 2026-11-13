package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
)

const (
	matrixPath = "../../build/runner-images/matrix.yaml"
	seedPath   = "../../internal/server/imagecatalog/catalog-seed.json"
)

// The checked-in seed is the generator's output for the checked-in matrix,
// byte for byte. A matrix change without `make image-manifest` fails here
// instead of shipping a catalog that no longer matches the images built.
func TestSeedMatchesMatrix(t *testing.T) {
	matrix, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("read matrix (update matrixPath and the Makefile's IMAGE_MATRIX if the topic moved): %v", err)
	}
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	var current manifest.Manifest
	if err := json.Unmarshal(seed, &current); err != nil {
		t.Fatal(err)
	}
	date, _, _ := strings.Cut(current.CatalogVersion, ".")
	got, err := generate(matrix, options{Registry: DefaultRegistry, Date: date})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seed) {
		t.Fatalf("%s is stale; run `make image-manifest`", filepath.Base(seedPath))
	}
}

func TestGenerateMapsMatrixItems(t *testing.T) {
	matrix, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generate(matrix, options{Registry: DefaultRegistry, Date: "2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(out)
	if err != nil {
		t.Fatalf("generated manifest does not parse: %v", err)
	}
	if m.SchemaVersion != 3 || !strings.HasPrefix(m.CatalogVersion, "2026-10-08.") || len(m.CatalogVersion) != len("2026-10-08.")+8 {
		t.Fatalf("schema/catalog version = %d %q", m.SchemaVersion, m.CatalogVersion)
	}
	versions := map[string]manifest.ManifestVersion{}
	seriesOf := map[string]manifest.ManifestSeries{}
	var names []string
	for _, series := range m.Series {
		names = append(names, series.Name)
		if series.Purpose != "inference" || len(series.CommonArgs) != 0 || len(series.CommonEnv) != 0 {
			t.Fatalf("series %q = %+v", series.Name, series)
		}
		for _, version := range series.Versions {
			versions[version.Name] = version
			seriesOf[version.Name] = series
		}
	}
	wantSeries := "MLX Audio · Metal,MLX-VLM · Metal,SGLang · CANN,SGLang · CUDA,SGLang · DTK,SGLang · HGGC,llama.cpp · Metal,vLLM Omni · CANN,vLLM Omni · CUDA,vLLM · CANN,vLLM · CUDA,vLLM · DTK,vLLM · HGGC,vLLM · Metal,vLLM · ROCm"
	if strings.Join(names, ",") != wantSeries {
		t.Fatalf("series = %v", names)
	}
	if len(versions) != 35 {
		t.Fatalf("versions = %d, want 35", len(versions))
	}
	// The Apple silicon environment package: framework metal, its version the
	// minimum macOS, its arch the CPU's.
	if metal := versions["metal15.0-vllm0.31.0-r2"]; metal.FrameworkVersion != "15.0" || metal.Arch != "arm64" || metal.EngineVersion != "0.31.0" || seriesOf["metal15.0-vllm0.31.0-r2"].Framework != "metal" {
		t.Fatalf("metal version = %+v in %+v", metal, seriesOf["metal15.0-vllm0.31.0-r2"])
	}
	// The servers the callable templates start are runners of engine callable.
	if llama := seriesOf["metal15.0-llama.cpp-b11617-r1"]; llama.Engine != "callable" || llama.Framework != "metal" || llama.Name != "llama.cpp · Metal" || versions["metal15.0-llama.cpp-b11617-r1"].EngineVersion != "b11617" {
		t.Fatalf("llama.cpp series = %+v, version %+v", llama, versions["metal15.0-llama.cpp-b11617-r1"])
	}
	if audio := seriesOf["metal15.0-mlx-audio0.5.9-r1"]; audio.Engine != "callable" || audio.Framework != "metal" || versions["metal15.0-mlx-audio0.5.9-r1"].EngineVersion != "0.5.9" {
		t.Fatalf("mlx-audio series = %+v", audio)
	}

	omni := versions["cann9.1-910b-vllm-omni0.30.0-r3"]
	if omni.ImageRef != "gpuworks/gpuworks-runner:cann9.1-910b-vllm-omni0.30.0-r3" || omni.EngineVersion != "0.30.0" || omni.FrameworkVersion != "9.1" || omni.Arch != "multi" {
		t.Fatalf("omni version = %+v", omni)
	}
	if s := seriesOf[omni.Name]; s.Engine != "vllm-omni" || s.Framework != "cann" || s.Name != "vLLM Omni · CANN" {
		t.Fatalf("omni series = %+v (the matrix says engine=vllm; the id decides)", s)
	}
	if !strings.Contains(omni.Notes, "requires: engine, omni, omni_no_mooncake, omni_patched, overlays") || !strings.HasSuffix(omni.Notes, "model: 910b") {
		t.Fatalf("omni notes = %q", omni.Notes)
	}
	wantSources := []string{
		"quay.io/gpuworks/gpuworks-runner:cann9.1-910b-vllm-omni0.30.0-r3",
		"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cann9.1-910b-vllm-omni0.30.0-r3",
	}
	if strings.Join(omni.RegistrySources, " ") != strings.Join(wantSources, " ") {
		t.Fatalf("registry sources = %v", omni.RegistrySources)
	}
	if dtk := versions["dtk26.04-vllm0.18.1-r2"]; dtk.Arch != "amd64" || dtk.FrameworkVersion != "26.04" || seriesOf[dtk.Name].Framework != "dtk" {
		t.Fatalf("dtk version = %+v", dtk)
	}
	if cuda := versions["cuda12.9-vllm0.31.0-r2"]; cuda.EngineVersion != "0.31.0" || cuda.FrameworkVersion != "12.9" || seriesOf[cuda.Name].Name != "vLLM · CUDA" {
		t.Fatalf("cuda12.9 version = %+v", cuda)
	}
	if baseline := versions["cann9.0-910b-vllm-omni0.23.0rc1-r1"]; baseline.EngineVersion != "0.23.0rc1" {
		t.Fatalf("revision r1 / rc engine version = %+v", baseline)
	}
	if sglang := versions["cann8.5-910b-sglang0.5.12.post1-r2"]; sglang.EngineVersion != "0.5.12.post1" || seriesOf[sglang.Name].Engine != "sglang" {
		t.Fatalf("post-release engine version = %+v", sglang)
	}
	for name, version := range versions {
		if version.RollingTag || version.Deprecated || version.BlobSHA256 != "" || version.SizeBytes != 0 {
			t.Fatalf("version %s carries a field the generator must not produce: %+v", name, version)
		}
	}
	again, err := generate(matrix, options{Registry: DefaultRegistry, Date: "2026-10-08"})
	if err != nil || !bytes.Equal(again, out) {
		t.Fatalf("generation is not reproducible: err=%v", err)
	}
}

// A repository gpuworks does not publish copies of has none to list.
func TestGenerateRequiresAPublishedRepository(t *testing.T) {
	_, err := generate([]byte("defaults:\n  platforms: [linux/amd64]\nitems:\n  - id: cuda13.0-vllm0.30.0\n    revision: r2\n"), options{Registry: "example/unmirrored", Date: "2026-10-08"})
	if err == nil || !strings.Contains(err.Error(), "no published copies") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseROCmCandidateTag(t *testing.T) {
	tag, err := parseTag("rocm7.2.4-gfx1151-sglang0.5.21")
	if err != nil || tag.Backend != "rocm" || tag.FrameworkVersion != "7.2.4" || tag.Model != "gfx1151" || tag.Engine != "sglang" || tag.EngineVersion != "0.5.21" {
		t.Fatalf("ROCm candidate tag = %+v, err = %v", tag, err)
	}
}

// The validated vLLM ROCm runner is listed; the SGLang candidate is not.
// Both stay pinned to their public amd64 base digests.
func TestROCmMatrixItemsPinPublicBases(t *testing.T) {
	data, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatal(err)
	}
	var matrix matrixFile
	if err := yaml.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		base   string
		listed bool
	}{
		"rocm7.2.3-vllm0.30.0":   {"vllm/vllm-openai-rocm@sha256:2e7da1ad1c66836802072588adea75f9f4991da5f9545b4318e91d422c22ce6a", true},
		"rocm7.2.0-sglang0.5.21": {"lmsysorg/sglang@sha256:0534eb382626ef55ace8be24f7d81cc6eb7451acac8a5727848397a5e9309d91", false},
	}
	for _, item := range matrix.Items {
		w, found := want[item.ID]
		if !found {
			continue
		}
		listed := item.Catalog == nil || *item.Catalog
		if listed != w.listed || item.Base != w.base || len(item.Platforms) != 1 || item.Platforms[0] != "linux/amd64" {
			t.Fatalf("ROCm item %s: listed=%v (want %v), %+v", item.ID, listed, w.listed, item)
		}
		delete(want, item.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing ROCm items: %v", want)
	}
}

func TestROCmRecipesContainRunnerVerificationContract(t *testing.T) {
	for _, engine := range []string{"vllm", "sglang"} {
		path := filepath.Join("../../build/runner-images/rocm", "Dockerfile."+engine)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{
			"ARG BASE_IMAGE", "FROM ${BASE_IMAGE}",
			"COPY shared/assert_requires.sh /opt/gpuworks/shared/assert_requires.sh",
			"COPY rocm/write_manifest.py /opt/gpuworks/rocm/write_manifest.py",
			"python3 /opt/gpuworks/amd-audit.py",
			"python3 /opt/gpuworks/rocm/write_manifest.py",
			"bash /opt/gpuworks/shared/assert_requires.sh",
		} {
			if !strings.Contains(string(data), required) {
				t.Errorf("%s missing %q", path, required)
			}
		}
	}
}

func TestGenerateRejectsInconsistentMatrix(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"id grammar": {
			yaml: "items:\n  - id: unknown6.4-vllm0.30.0\n    revision: r1\n",
			want: "unknown6.4-vllm0.30.0",
		},
		"engine version arg": {
			yaml: "items:\n  - id: cuda13.0-vllm0.30.0\n    revision: r2\n    args:\n      ENGINE_VERSION: \"0.29.0\"\n",
			want: "cuda13.0-vllm0.30.0",
		},
		"sglang version arg": {
			yaml: "items:\n  - id: cann9.0-910b-sglang0.5.18\n    revision: r2\n    args:\n      SGLANG_VERSION: \"0.5.17\"\n",
			want: "cann9.0-910b-sglang0.5.18",
		},
		"framework version arg": {
			yaml: "items:\n  - id: cuda12.9-vllm0.31.0\n    revision: r2\n    args:\n      FRAMEWORK_VERSION: \"12.8\"\n",
			want: "cuda12.9-vllm0.31.0",
		},
		"platform": {
			yaml: "defaults:\n  platforms: [linux/amd64]\nitems:\n  - id: cuda13.0-vllm0.30.0\n    revision: r2\n    platforms: [linux/amd64, linux/ppc64le]\n",
			want: "cuda13.0-vllm0.30.0",
		},
		"revision": {
			yaml: "items:\n  - id: cuda13.0-vllm0.30.0\n",
			want: "cuda13.0-vllm0.30.0",
		},
		"backend": {
			yaml: "items:\n  - id: cuda13.0-vllm0.30.0\n    revision: r2\n    backend: cann\n",
			want: "cuda13.0-vllm0.30.0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := generate([]byte("defaults:\n  platforms: [linux/amd64]\n"+strings.TrimPrefix(tc.yaml, "defaults:\n  platforms: [linux/amd64]\n")), options{Date: "2026-10-08"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// The omni engine must win over plain vllm, or omni ids would split into
// engine=vllm with an engine version of "-omni…".
// llama.cpp numbers its releases b<build>; the id writes them llama.cpp-b<build>.
func TestParseTagReadsALlamaCppBuild(t *testing.T) {
	tag, err := parseTag("metal15.0-llama.cpp-b11617")
	if err != nil || tag.Engine != "llama.cpp" || tag.EngineVersion != "b11617" || tag.Backend != "metal" || tag.FrameworkVersion != "15.0" {
		t.Fatalf("tag = %+v, %v", tag, err)
	}
	for _, bad := range []string{"metal15.0-llama.cpp11617", "metal15.0-llama.cpp-11617", "metal15.0-llama.cppb11617"} {
		if _, err := parseTag(bad); err == nil {
			t.Errorf("parseTag(%q) accepted a malformed llama.cpp id", bad)
		}
	}
}

func TestParseTagPrefersOmni(t *testing.T) {
	tag, err := parseTag("cann9.1-910b-vllm-omni0.30.0")
	if err != nil {
		t.Fatal(err)
	}
	if tag != (parsedTag{Backend: "cann", FrameworkVersion: "9.1", Model: "910b", Engine: "vllm-omni", EngineVersion: "0.30.0"}) {
		t.Fatalf("tag = %+v", tag)
	}
}

// The metal framework version of an environment package is the minimum macOS
// it runs on, which the agent's doctor and the install refuse below
// (accelerator.MinimumMacOSMajor), and build.sh builds for (MACOS_TARGET).
// The three cannot drift apart.
func TestMetalFrameworkVersionIsTheMinimumMacOS(t *testing.T) {
	data, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatal(err)
	}
	var matrix matrixFile
	if err := yaml.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	want := strconv.Itoa(accelerator.MinimumMacOSMajor) + ".0"
	metal := 0
	for _, item := range matrix.Items {
		tag, err := parseTag(item.ID)
		if err != nil || tag.Backend != "metal" {
			continue
		}
		metal++
		if tag.FrameworkVersion != want {
			t.Errorf("%s: framework version %s, want the minimum macOS %s", item.ID, tag.FrameworkVersion, want)
		}
	}
	if metal == 0 {
		t.Fatal("no metal items in the matrix")
	}
	script, err := os.ReadFile("../../build/host-envs/build.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^MACOS_TARGET=(\S+)`).FindSubmatch(script)
	if m == nil || string(m[1]) != want {
		t.Fatalf("build.sh MACOS_TARGET = %q, want %s", m, want)
	}
}

// The V100 line carries a model slot of its own and an engine version that is
// the fork's: 1Cat-vLLM numbers its releases itself.
func TestParseTagReadsTheVoltaLine(t *testing.T) {
	tag, err := parseTag("cuda12.8-v100-vllm1cat1.5.1")
	if err != nil {
		t.Fatal(err)
	}
	if tag != (parsedTag{Backend: "cuda", FrameworkVersion: "12.8", Model: "v100", Engine: "vllm", EngineVersion: "1cat1.5.1"}) {
		t.Fatalf("tag = %+v", tag)
	}
}

// catalog: false builds an image without offering it: the automatic image pick
// sees only the catalog, and it never looks at the GPU model (ADR 0019), so a
// Volta-only image listed there would be chosen (or missed) for the wrong
// nodes. The id is still validated, so a typo cannot hide behind the flag.
func TestGenerateLeavesUnlistedItemsOutOfTheCatalog(t *testing.T) {
	const listed = "  - id: cuda12.9-vllm0.31.0\n    revision: r2\n"
	const unlisted = "  - id: cuda12.8-v100-vllm1cat1.5.1\n    revision: r1\n    catalog: false\n    args:\n      ENGINE_VERSION: \"1cat1.5.1\"\n      FRAMEWORK_VERSION: \"12.8\"\n"
	head := "defaults:\n  platforms: [linux/amd64]\nitems:\n"
	with, err := generate([]byte(head+listed+unlisted), options{Date: "2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	without, err := generate([]byte(head+listed), options{Date: "2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(with, without) {
		t.Fatalf("an unlisted item changed the catalog:\n%s\nvs\n%s", with, without)
	}
	if bytes.Contains(with, []byte("v100")) {
		t.Fatalf("an unlisted item reached the catalog: %s", with)
	}
	// Listed explicitly, the same item is a catalog entry in the vLLM CUDA series.
	explicit := strings.Replace(unlisted, "catalog: false", "catalog: true", 1)
	out, err := generate([]byte(head+listed+explicit), options{Date: "2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, series := range parsed.Series {
		for _, version := range series.Versions {
			if version.Name == "cuda12.8-v100-vllm1cat1.5.1-r1" {
				found = version.EngineVersion == "1cat1.5.1" && version.FrameworkVersion == "12.8" && series.Name == "vLLM · CUDA" && strings.HasSuffix(version.Notes, "model: v100")
			}
		}
	}
	if !found {
		t.Fatalf("catalog: true did not list the V100 item: %s", out)
	}
	_, err = generate([]byte(head+"  - id: cuda12.8-v200-vllm1cat1.5.1\n    revision: r1\n    catalog: false\n"), options{Date: "2026-10-08"})
	if err == nil || !strings.Contains(err.Error(), "v200") {
		t.Fatalf("an unlisted item with a bad id was accepted: %v", err)
	}
}

// The checked-in matrix declares the V100 line and keeps it out of the seed.
func TestMatrixDeclaresTheVoltaImageUnlisted(t *testing.T) {
	data, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatal(err)
	}
	var matrix matrixFile
	if err := yaml.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	var found *matrixItem
	for i := range matrix.Items {
		if matrix.Items[i].ID == "cuda12.8-v100-vllm1cat1.5.1" {
			found = &matrix.Items[i]
		}
	}
	if found == nil {
		t.Fatal("the matrix has no V100 item")
	}
	if found.Catalog == nil || *found.Catalog {
		t.Fatalf("the V100 item must stay unlisted until it has run on hardware: %+v", found.Catalog)
	}
	if found.Args["ENGINE_VERSION"] != "1cat1.5.1" || found.Args["ONECAT_VERSION"] != "1.5.1" || len(found.Args["ONECAT_WHEEL_SHA256"]) != 64 {
		t.Fatalf("args = %v", found.Args)
	}
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(seed, []byte("v100")) {
		t.Fatal("the seed lists the V100 image")
	}
}
