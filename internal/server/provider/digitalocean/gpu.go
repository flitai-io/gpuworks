package digitalocean

import (
	"regexp"
	"strconv"
	"strings"
)

// gpuInfo is the size object's gpu_info:
// {"count":1,"vram":{"amount":20,"unit":"gib"},"model":"nvidia_rtx4000_ada"}.
type gpuInfo struct {
	Count int    `json:"count"`
	Model string `json:"model"`
	VRAM  struct {
		Amount float64 `json:"amount"`
		Unit   string  `json:"unit"`
	} `json:"vram"`
}

// readableModel turns the API's model id into a name: "nvidia_rtx4000_ada"
// is "NVIDIA RTX 4000 Ada", "amd_mi300x" is "AMD MI300X".
func readableModel(id string) string {
	var words []string
	for _, part := range strings.Split(strings.ToLower(strings.TrimSpace(id)), "_") {
		if part == "" {
			continue
		}
		words = append(words, modelWord(part))
	}
	return strings.Join(words, " ")
}

var seriesNumber = regexp.MustCompile(`^(rtx|gtx|rx)(\d+)$`)

func modelWord(w string) string {
	switch w {
	case "nvidia":
		return "NVIDIA"
	case "amd":
		return "AMD"
	}
	// "rtx4000" is "RTX 4000"; a data-center card such as "h100" keeps its
	// letter and digits together.
	if m := seriesNumber.FindStringSubmatch(w); m != nil {
		return strings.ToUpper(m[1]) + " " + m[2]
	}
	if isWord(w) {
		return strings.ToUpper(w[:1]) + w[1:] // "ada" -> "Ada"
	}
	return strings.ToUpper(w)
}

func isWord(w string) bool {
	switch w {
	case "ada", "hopper", "blackwell", "lovelace", "ampere", "turing", "volta", "pcie", "sxm":
		return true
	}
	return false
}

// sizeSlug is a GPU size slug taken apart: "gpu-h100x8-640gb" is model
// "H100" x8, "gpu-mi300x1-192gb" is "MI300X" x1.
type sizeSlug struct {
	Model  string // "H100", "RTX 6000 Ada"
	Count  int
	Vendor string // "nvidia" | "amd"
}

// nvidiaSlugModels names the model tokens of NVIDIA sizes that need more
// than upper-casing.
var nvidiaSlugModels = map[string]string{
	"4000ada": "RTX 4000 Ada",
	"5000ada": "RTX 5000 Ada",
	"6000ada": "RTX 6000 Ada",
}

var (
	slugToken    = regexp.MustCompile(`^gpu-([a-z0-9]+)(?:-|$)`)
	slugTrailing = regexp.MustCompile(`^(.+?)(\d+)$`)
	amdToken     = regexp.MustCompile(`^mi\d`)
	adaToken     = regexp.MustCompile(`^(\d+)ada$`)
)

// parseSizeSlug reads the model and GPU count out of a GPU size slug, for
// sizes the API's catalog does not show to the account. The first dash
// separated word is the model and the count ("h100x8", "6000adax1",
// "mi300x1"); what follows (memory, "spot", "lc") is not needed.
func parseSizeSlug(slug string) (sizeSlug, bool) {
	m := slugToken.FindStringSubmatch(strings.ToLower(strings.TrimSpace(slug)))
	if m == nil {
		return sizeSlug{}, false
	}
	tm := slugTrailing.FindStringSubmatch(m[1])
	if tm == nil {
		return sizeSlug{}, false
	}
	count, err := strconv.Atoi(tm[2])
	if err != nil || count <= 0 {
		return sizeSlug{}, false
	}
	model := tm[1]
	if amdToken.MatchString(model) {
		// "mi300x" is the model, the card's own X, followed by the count.
		return sizeSlug{Model: strings.ToUpper(model), Count: count, Vendor: "amd"}, true
	}
	// "h100x" + "8": the x separates the count from the model.
	model, hasX := strings.CutSuffix(model, "x")
	if !hasX || model == "" {
		return sizeSlug{}, false
	}
	if name, ok := nvidiaSlugModels[model]; ok {
		return sizeSlug{Model: name, Count: count, Vendor: "nvidia"}, true
	}
	if a := adaToken.FindStringSubmatch(model); a != nil {
		return sizeSlug{Model: "RTX " + a[1] + " Ada", Count: count, Vendor: "nvidia"}, true
	}
	return sizeSlug{Model: strings.ToUpper(model), Count: count, Vendor: "nvidia"}, true
}

// isGPUSlug: a size DigitalOcean sells as a GPU droplet.
func isGPUSlug(slug string) bool { return strings.HasPrefix(strings.ToLower(slug), "gpu-") }

// vendorOf is the accelerator vendor of a model id from the API
// ("nvidia_h100", "amd_mi300x"); "" when the id does not say.
func vendorOf(modelID string) string {
	id := strings.ToLower(modelID)
	switch {
	case strings.HasPrefix(id, "nvidia"):
		return "nvidia"
	case strings.HasPrefix(id, "amd"):
		return "amd"
	}
	return ""
}
