package inferencecatalog

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// EditableYAML renders one stored model as the flat YAML document the template
// editor shows and the import endpoint takes back.
//
// The text is produced by marshaling the very types the importer decodes
// (FlatCatalog), so a field the catalog format has cannot be lost on the way
// out, and no default is invented for one the model leaves empty.
//
// specSource, when not empty, keeps only the specs of that source. The editor
// asks for "user" on an upstream model: the user's specs are the only ones an
// import may replace. The model's documentation is written only when the
// editor's save can own it: either every spec is included, or the model itself
// is user-authored. An upstream model's README files stay with its package.
func EditableYAML(row store.InferenceModel, specSource string) ([]byte, error) {
	model, err := FromFlatStoreModel(row)
	if err != nil {
		return nil, err
	}
	ownsDocumentation := specSource == "" || row.Source == "user"
	model.Source = "user"
	if !ownsDocumentation {
		model.ReadmeZHCN, model.ReadmeEN = "", ""
	}
	if specSource != "" {
		kept := make([]InferenceSpec, 0, len(model.Specs))
		for index, spec := range model.Specs {
			if row.Specs[index].Source == specSource {
				kept = append(kept, spec)
			}
		}
		model.Specs = kept
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(FlatCatalog{Models: []InferenceModel{model}}); err != nil {
		return nil, fmt.Errorf("marshal inference template %s YAML: %w", row.ModelID, err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("marshal inference template %s YAML: %w", row.ModelID, err)
	}
	return out.Bytes(), nil
}
