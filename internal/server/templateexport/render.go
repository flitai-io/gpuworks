package templateexport

import (
	"fmt"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// renderYAML writes model as a flat catalog document headed by comments that
// say where it came from, what was verified and what a reviewer has to fill
// in. Comments are ignored by the parser, so the document imports exactly as
// the model it describes; they are in English because the file is meant for
// the catalog's maintainers, whose model.yaml comments are English too.
func renderYAML(model inferencecatalog.InferenceModel, in Input, notes []Note) (string, error) {
	data, err := inferencecatalog.MarshalYAML(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# Saved from GPUWorks deployment %q (id %d).\n", in.DeploymentName, in.DeploymentID)
	for _, note := range notes {
		for _, line := range strings.Split(noteText(note), "\n") {
			out.WriteString("# " + line + "\n")
		}
	}
	out.Write(data)
	return out.String(), nil
}

// renderEvidence is the test record a contributor attaches when proposing the
// template: the cards, image and engine, and every note.
func renderEvidence(in Input, notes []Note) string {
	var out strings.Builder
	card := in.GPUs[0]
	fmt.Fprintf(&out, "GPUWorks template test record\n\n")
	fmt.Fprintf(&out, "- Deployment: %s (id %d)\n", in.DeploymentName, in.DeploymentID)
	fmt.Fprintf(&out, "- Cards: %d x %s (%s, %d MiB", in.GPUCount, card.Name, card.Vendor, card.MemoryMiB)
	if card.ComputeCap != "" {
		fmt.Fprintf(&out, ", compute capability %s", card.ComputeCap)
	}
	if card.DriverVersion != "" {
		fmt.Fprintf(&out, ", driver %s", card.DriverVersion)
	}
	out.WriteString(")\n")
	fmt.Fprintf(&out, "- Engine: %s\n- Image: %s\n\n", in.Engine, in.Image)
	for _, note := range notes {
		out.WriteString("- " + strings.ReplaceAll(noteText(note), "\n", "\n  ") + "\n")
	}
	return out.String()
}

// noteText is a note in English, for the file and the test record. Clients
// render the same note in their own language from its code and params.
func noteText(note Note) string {
	p := note.Params
	switch note.Code {
	case NoteVerifiedOn:
		return fmt.Sprintf("Verified on %v x %v.", p["count"], p["gpu"])
	case NoteLocalWeights:
		return fmt.Sprintf("Local weights %v must already be registered on the destination cluster; this template does not bundle model files.", p["repo_id"])
	case NoteOperationPassed:
		return fmt.Sprintf("Tested %v: HTTP %v.", p["operation"], p["http_status"])
	case NoteOperationFailed:
		return fmt.Sprintf("FAILED %v: HTTP %v %v", p["operation"], p["http_status"], p["message"])
	case NoteOperationNotTested:
		return fmt.Sprintf("Not tested: %v (no test request is sent for this operation).", p["operation"])
	case NoteKVBytesMissing:
		return fmt.Sprintf("Missing kv_bytes_per_token for %v: compute it from config.json\n(scripts/estimate-calib/kv_layout.py) before proposing this template.", p["engine"])
	case NoteModelConfigMissing:
		return "Missing model_config: config.json was not available."
	case NotePlacementOmitted:
		return fmt.Sprintf("No placement: the deployment ran on %v card(s) with %v MiB each.", p["gpu_count"], p["gpu_memory_mib"])
	case NoteVRAMLimit:
		return fmt.Sprintf("The deployment ran under a dGPU memory limit of %v MiB per card.", p["vram_limit_mib"])
	case NoteEnvExcluded:
		return fmt.Sprintf("Deployment env not exported: %s.", joinKeys(p["keys"]))
	case NoteParametersDropped:
		return fmt.Sprintf("Settings no engine flag carries, not exported: %s.", joinKeys(p["keys"]))
	case NoteContainerOverridesOmitted:
		return "Container overrides (mounts, docker args) are not exported."
	}
	return note.Code
}

func joinKeys(value any) string {
	keys, _ := value.([]string)
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	return strings.Join(sorted, ", ")
}
