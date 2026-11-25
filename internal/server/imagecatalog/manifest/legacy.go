package manifest

import (
	"encoding/json"
	"strings"
)

// LegacyCatalog produces the schema 3 catalog for servers that use size_bytes
// for unpacked disk admission. Such servers reconcile by exact ref; keeping
// retired names also preserves their local archive URLs and enable switches.
func LegacyCatalog(data []byte) ([]byte, error) {
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if m.SchemaVersion < 4 {
		return data, nil
	}
	m.SchemaVersion = 3
	listed := make(map[string]bool)
	for _, s := range m.Series {
		for _, v := range s.Versions {
			listed[v.ImageRef] = true
		}
	}
	for i := range m.Series {
		s := &m.Series[i]
		var versions []ManifestVersion
		for _, v := range s.Versions {
			v.DownloadBytes = 0
			// Schema 3 consumers only have rolling_tag to exclude a manual entry.
			v.RollingTag = v.RollingTag || v.ManualOnly
			v.ManualOnly = false
			if s.Engine == "vllm-omni" && strings.Contains(v.Name, "-omni-") {
				// Older deployers also use this value to gate the base vLLM CLI.
				v.EngineVersion = ""
			}
			versions = append(versions, v)
			aliases, err := RetiredRunnerAliases(v.ImageRef)
			if err != nil {
				return nil, err
			}
			for _, alias := range aliases {
				old := v
				old.ImageRef = alias.ImageRef
				if listed[old.ImageRef] {
					continue
				}
				listed[old.ImageRef] = true
				old.Name, old.Arch = alias.ImageRef[strings.LastIndexByte(alias.ImageRef, ':')+1:], alias.Arch
				old.RollingTag = true
				old.RegistrySources = []string{v.ImageRef}
				old.Notes = "Retired image tag. Pull its release mirror or use the existing cluster archive."
				versions = append(versions, old)
			}
		}
		s.Versions = versions
	}
	m.CatalogVersion += ".legacy"
	m.Signature = ""
	m.SHA256 = ComputeSHA256(m)
	if err := m.Validate(); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
