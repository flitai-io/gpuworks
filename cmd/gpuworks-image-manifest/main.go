// gpuworks-image-manifest turns the gpuworks-runner build matrix into the
// system image catalog manifest. The same output is embedded as the server's
// catalog seed and published as the upstream manifest the "check for updates"
// action fetches, so one generator run feeds both.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
)

// DefaultRegistry is the canonical repository written into image_ref.
const DefaultRegistry = "gpuworks/gpuworks-runner"

// tagPattern is the matrix id grammar: <backend><framework version>[-<model>]-<engine><engine version>.
// vllm-omni must precede vllm in the alternation, or the omni ids would split
// into engine=vllm with an engine version of "-omni…". llama.cpp numbers its
// releases b<build> and is written llama.cpp-b<build>.
var tagPattern = regexp.MustCompile(`^(?P<backend>cuda|cann|dtk|hggc|metal|rocm)(?P<fwver>[0-9]+(\.[0-9]+)*)(-(?P<model>910b|a3|310p|950|v100|gfx1151))?-(?:(?P<engine>` +
	regexp.QuoteMeta(engines.VLLMOmni) + `|` + regexp.QuoteMeta(engines.VLLM) + `|` + regexp.QuoteMeta(engines.SGLang) +
	`|mlx-vlm|mlx-audio|systemone)(?P<engver>[0-9][0-9A-Za-z.]*)|(?P<llamacpp>llama\.cpp)-(?P<llamabuild>b[0-9]+))$`)

// callableServers are the runner tags whose engine is a server the inference
// templates start through the callable engine (with the gpuworks adapter in
// the package), named the way their projects write them.
var callableServers = map[string]string{"mlx-vlm": "MLX-VLM", "mlx-audio": "MLX Audio", "llama.cpp": "llama.cpp", "systemone": "System One"}

// catalogEngine is the image catalog's engine for a tag's engine.
func catalogEngine(tagEngine string) string {
	if _, ok := callableServers[tagEngine]; ok {
		return engines.Callable
	}
	return tagEngine
}

func main() {
	inPath := flag.String("in", "", "gpuworks-runner matrix.yaml")
	outPath := flag.String("out", "-", "output manifest JSON")
	registry := flag.String("registry", DefaultRegistry, "canonical image repository")
	date := flag.String("date", "", "catalog date (YYYY-MM-DD); defaults to today")
	verifyPath := flag.String("verify", "", "instead of generating, check that every reference in this manifest pulls whole from every registry it lists")
	flag.Parse()
	if strings.TrimSpace(*verifyPath) != "" {
		os.Exit(runVerify(*verifyPath))
	}
	if strings.TrimSpace(*inPath) == "" {
		fatal(fmt.Errorf("-in is required"))
	}
	data, err := os.ReadFile(*inPath)
	if err != nil {
		fatal(err)
	}
	catalogDate := strings.TrimSpace(*date)
	if catalogDate == "" {
		catalogDate = time.Now().UTC().Format("2006-01-02")
	}
	out, err := generate(data, options{Registry: *registry, Date: catalogDate})
	if err != nil {
		fatal(err)
	}
	if err := writePath(*outPath, out); err != nil {
		fatal(err)
	}
}

type options struct {
	Registry string
	Date     string
}

type matrixFile struct {
	Defaults struct {
		Platforms []string `yaml:"platforms"`
	} `yaml:"defaults"`
	Items []matrixItem `yaml:"items"`
}

type matrixItem struct {
	ID        string            `yaml:"id"`
	Revision  string            `yaml:"revision"`
	Backend   string            `yaml:"backend"`
	Base      string            `yaml:"base"`
	Platforms []string          `yaml:"platforms"`
	Args      map[string]string `yaml:"args"`
	Requires  []string          `yaml:"requires"`
	Note      string            `yaml:"note"`
	// Catalog false keeps a built image out of the system image catalog: the
	// recipe is built, verified and published like any other, but the product
	// does not offer it (and the automatic image pick never sees it) until it
	// has run on real hardware and an inference template names it. Absent
	// means listed. The id is still validated either way.
	Catalog *bool `yaml:"catalog"`
}

// parsedTag is what an id says about an image. The matrix's own engine and
// args fields are build inputs and are only cross-checked, never copied: the
// omni items declare engine=vllm, and the CANN args carry no framework version.
type parsedTag struct {
	Backend, FrameworkVersion, Model, Engine, EngineVersion string
}

func parseTag(id string) (parsedTag, error) {
	match := tagPattern.FindStringSubmatch(id)
	if match == nil {
		return parsedTag{}, fmt.Errorf("matrix id %q does not match <backend><version>[-<model>]-<engine><version>", id)
	}
	group := func(name string) string { return match[tagPattern.SubexpIndex(name)] }
	tag := parsedTag{Backend: group("backend"), FrameworkVersion: group("fwver"), Model: group("model"), Engine: group("engine"), EngineVersion: group("engver")}
	if group("llamacpp") != "" {
		tag.Engine, tag.EngineVersion = group("llamacpp"), group("llamabuild")
	}
	return tag, nil
}

func generate(data []byte, opt options) ([]byte, error) {
	var matrix matrixFile
	if err := yaml.Unmarshal(data, &matrix); err != nil {
		return nil, fmt.Errorf("parse matrix: %w", err)
	}
	if len(matrix.Items) == 0 {
		return nil, fmt.Errorf("matrix has no items")
	}
	registry := strings.TrimSuffix(strings.TrimSpace(opt.Registry), "/")
	if registry == "" {
		registry = DefaultRegistry
	}
	seriesByName := map[string]*manifest.ManifestSeries{}
	for _, item := range matrix.Items {
		version, tag, err := versionFromItem(item, matrix.Defaults.Platforms, registry)
		if err != nil {
			return nil, err
		}
		if item.Catalog != nil && !*item.Catalog {
			continue
		}
		name := seriesName(tag.Engine, tag.Backend)
		series := seriesByName[name]
		if series == nil {
			series = &manifest.ManifestSeries{Name: name, Engine: catalogEngine(tag.Engine), Purpose: "inference", Framework: tag.Backend, CommonArgs: []manifest.CommonArg{}, CommonEnv: []manifest.CommonEnv{}}
			seriesByName[name] = series
		}
		series.Versions = append(series.Versions, version)
	}
	series := make([]manifest.ManifestSeries, 0, len(seriesByName))
	for _, value := range seriesByName {
		sort.Slice(value.Versions, func(i, j int) bool { return value.Versions[i].ImageRef < value.Versions[j].ImageRef })
		series = append(series, *value)
	}
	sort.Slice(series, func(i, j int) bool { return series[i].Name < series[j].Name })
	seriesJSON, err := json.Marshal(series)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(seriesJSON)
	m := manifest.Manifest{SchemaVersion: 3, CatalogVersion: opt.Date + "." + hex.EncodeToString(sum[:])[:8], Series: series}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	m.SHA256 = manifest.ComputeSHA256(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func versionFromItem(item matrixItem, defaultPlatforms []string, registry string) (manifest.ManifestVersion, parsedTag, error) {
	id := strings.TrimSpace(item.ID)
	tag, err := parseTag(id)
	if err != nil {
		return manifest.ManifestVersion{}, tag, err
	}
	if accelerator.FrameworkVendor(tag.Backend) == "" {
		return manifest.ManifestVersion{}, tag, fmt.Errorf("matrix id %q: framework %q has no accelerator vendor", id, tag.Backend)
	}
	if backend := strings.TrimSpace(item.Backend); backend != "" && backend != tag.Backend {
		return manifest.ManifestVersion{}, tag, fmt.Errorf("matrix id %q: backend %q disagrees with the id", id, backend)
	}
	// SGLANG_VERSION is a build input only on sglang items; on a vllm item it
	// would name something else, so only the engine's own key is compared.
	versionArg := "ENGINE_VERSION"
	if tag.Engine == engines.SGLang {
		versionArg = "SGLANG_VERSION"
	}
	if value, ok := item.Args[versionArg]; ok && value != tag.EngineVersion {
		return manifest.ManifestVersion{}, tag, fmt.Errorf("matrix id %q: args.%s=%q disagrees with the id's engine version %q", id, versionArg, value, tag.EngineVersion)
	}
	if value, ok := item.Args["FRAMEWORK_VERSION"]; ok && value != tag.FrameworkVersion {
		return manifest.ManifestVersion{}, tag, fmt.Errorf("matrix id %q: args.FRAMEWORK_VERSION=%q disagrees with the id's framework version %q", id, value, tag.FrameworkVersion)
	}
	revision := strings.TrimSpace(item.Revision)
	if revision == "" {
		return manifest.ManifestVersion{}, tag, fmt.Errorf("matrix id %q: revision is required", id)
	}
	platforms := item.Platforms
	if len(platforms) == 0 {
		platforms = defaultPlatforms
	}
	arch, err := archFromPlatforms(platforms)
	if err != nil {
		return manifest.ManifestVersion{}, tag, fmt.Errorf("matrix id %q: %w", id, err)
	}
	immutable := id + "-" + revision
	ref := registry + ":" + immutable
	sources, err := mirrorSources(ref)
	if err != nil {
		return manifest.ManifestVersion{}, tag, fmt.Errorf("matrix id %q: %w", id, err)
	}
	return manifest.ManifestVersion{
		Name:             immutable,
		ImageRef:         ref,
		EngineVersion:    tag.EngineVersion,
		FrameworkVersion: tag.FrameworkVersion,
		Arch:             arch,
		RegistrySources:  sources,
		Notes:            versionNotes(item, tag),
	}, tag, nil
}

// publishedRegistries are the public registries every runner tag is copied
// to besides Docker Hub, byte for byte (the release pipeline pushes them with
// skopeo). registry_sources carries them to every installation as copies a
// node may pull instead; which it tries first is the node's (it probes them),
// unless an operator's pull source rule says otherwise. An internal registry
// the release also pushes to is not public and is not listed: an installation
// that can reach it says so with a rule of its own.
var publishedRegistries = []string{
	"quay.io/gpuworks/gpuworks-runner",
	"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner",
}

// mirrorSources lists the public copies of a runner tag.
func mirrorSources(ref string) ([]string, error) {
	repo, tag, ok := strings.Cut(ref, ":")
	if !ok || repo != DefaultRegistry || tag == "" {
		return nil, fmt.Errorf("no published copies are known for %s", ref)
	}
	out := make([]string, 0, len(publishedRegistries))
	for _, published := range publishedRegistries {
		out = append(out, published+":"+tag)
	}
	return out, nil
}

func archFromPlatforms(platforms []string) (string, error) {
	set := map[string]bool{}
	for _, platform := range platforms {
		switch strings.TrimSpace(platform) {
		case "linux/amd64":
			set["amd64"] = true
		case "linux/arm64", "darwin/arm64":
			// A darwin/arm64 item is an environment package for Apple silicon
			// (framework metal); the catalog's arch is the CPU architecture.
			set["arm64"] = true
		default:
			return "", fmt.Errorf("unsupported platform %q", platform)
		}
	}
	switch {
	case set["amd64"] && set["arm64"]:
		return "multi", nil
	case set["amd64"]:
		return "amd64", nil
	case set["arm64"]:
		return "arm64", nil
	default:
		return "", fmt.Errorf("no platforms")
	}
}

// versionNotes keeps the matrix's own description. The device model only
// lives in the tag, so it is spelled out here rather than added as a field.
func versionNotes(item matrixItem, tag parsedTag) string {
	lines := []string{}
	if note := strings.TrimSpace(item.Note); note != "" {
		lines = append(lines, note)
	}
	if len(item.Requires) > 0 {
		lines = append(lines, "requires: "+strings.Join(item.Requires, ", "))
	}
	if tag.Model != "" {
		lines = append(lines, "model: "+tag.Model)
	}
	return strings.Join(lines, "\n")
}

// seriesName writes a framework the way its vendor does: the acronyms (CUDA,
// CANN, DTK, HGGC) in capitals, Apple's Metal as a word and AMD's ROCm in
// its own casing.
func seriesName(engine, framework string) string {
	name := strings.ToUpper(framework)
	switch framework {
	case "metal":
		name = "Metal"
	case "rocm":
		name = "ROCm"
	}
	display := engines.DisplayName(engine)
	if server, ok := callableServers[engine]; ok {
		display = server
	}
	return display + " · " + name
}

func writePath(path string, b []byte) error {
	if path == "-" {
		_, err := os.Stdout.Write(b)
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// runVerify prints every problem the pre-publish check finds and returns the
// process exit code.
func runVerify(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	report, err := verifyManifest(ctx, data, newVerifier(nil, nil))
	if err != nil {
		fatal(err)
	}
	for _, problem := range report.Problems {
		fmt.Fprintln(os.Stderr, "FAIL", problem)
	}
	fmt.Printf("checked %d references: %d platform manifests, %d blobs, %d problems\n",
		report.Refs, report.Platforms, report.Blobs, len(report.Problems))
	if len(report.Problems) > 0 {
		return 1
	}
	return 0
}

func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
