package inferencecatalog

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skipped is a model, or one spec of a model, that a tolerant load left out
// of an official catalog package because this build cannot read or validate
// it. Templates are published against the newest server, so the usual cause
// is a field or a rule this build predates.
type Skipped struct {
	ModelID string
	// Spec is empty when the whole model was left out.
	Spec   string
	Reason string
}

// LoadPackageTolerant reads an official catalog package the way LoadPackage
// does, but leaves out the models and specs this build cannot read or
// validate instead of refusing the package: one template written for a newer
// server must not cost an older one every other update. The layout, the
// manifest counts and the assets of what is kept are checked as strictly as
// ever, so a damaged package is still refused. A user's own import stays on
// LoadPackage and DecodeYAML: there an unknown field is a mistake to report.
func LoadPackageTolerant(fsys fs.FS) (Package, []Skipped, error) {
	return loadPackage(fsys, true)
}

// ReadPackageArchiveTolerant is ReadPackageArchive with LoadPackageTolerant.
func ReadPackageArchiveTolerant(reader io.Reader) (Archive, []Skipped, error) {
	return readPackageArchive(reader, true)
}

// skipReason keeps the first line of a decode or validation error: a YAML
// error repeats itself once per offending line.
func skipReason(err error) string {
	reason, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	reason = strings.TrimSuffix(strings.TrimSpace(reason), ":")
	if rest := strings.TrimSpace(strings.TrimPrefix(err.Error(), reason+":")); reason == "yaml: unmarshal errors" && rest != "" {
		reason, _, _ = strings.Cut(rest, "\n")
	}
	return strings.TrimSpace(reason)
}

// parseModelTolerant reads a model.yaml the strict parser refused, one spec
// at a time. It returns the model with the specs that parse, the number of
// specs the file states, and what it left out; a nil model means the model's
// own fields do not parse. ok is false when the file is not a model document
// at all, which no build could read.
func parseModelTolerant(name string, data []byte) (model *InferenceModel, stated int, skipped []Skipped, ok bool) {
	var document yaml.Node
	if yaml.Unmarshal(data, &document) != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, 0, nil, false
	}
	// A spec may reuse a block another spec anchors; each spec is parsed on
	// its own below, so every alias is replaced by what it stands for first.
	resolveAliases(document.Content[0], 0)
	root := document.Content[0]
	modelID := strings.ToLower(strings.TrimSpace(yamlString(root, "model_id")))
	if modelID == "" {
		modelID = strings.TrimPrefix(path.Dir(name), "models/")
	}
	var specs []*yaml.Node
	if list := yamlMappingValue(root, "specs"); list != nil {
		if list.Kind != yaml.SequenceNode {
			return nil, 0, nil, false
		}
		specs = list.Content
	}
	base, err := parseModelNode(root, nil)
	if err != nil {
		return nil, len(specs), []Skipped{{ModelID: modelID, Reason: skipReason(err)}}, true
	}
	for _, spec := range specs {
		one, err := parseModelNode(root, spec)
		if err != nil || len(one.Specs) != 1 {
			if err == nil {
				err = fmt.Errorf("spec does not decode to one spec")
			}
			skipped = append(skipped, Skipped{ModelID: modelID, Spec: strings.TrimSpace(yamlString(spec, "name")), Reason: skipReason(err)})
			continue
		}
		base.Specs = append(base.Specs, one.Specs[0])
	}
	return &base, len(specs), skipped, true
}

// resolveAliases replaces every alias under node by the node it refers to and
// drops the anchors, so any subtree can be marshalled without the rest of the
// document. depth bounds a document that aliases itself.
func resolveAliases(node *yaml.Node, depth int) {
	if node == nil || depth > 64 {
		return
	}
	node.Anchor = ""
	for index, child := range node.Content {
		if child.Kind == yaml.AliasNode && child.Alias != nil {
			target := *child.Alias
			target.Anchor = ""
			node.Content[index] = &target
			child = node.Content[index]
		}
		resolveAliases(child, depth+1)
	}
}

// parseModelNode strictly parses the model document root with its specs
// replaced by the one given, or by none.
func parseModelNode(root, spec *yaml.Node) (InferenceModel, error) {
	only := &yaml.Node{Kind: yaml.MappingNode, Tag: root.Tag}
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == "specs" {
			continue
		}
		only.Content = append(only.Content, root.Content[index], root.Content[index+1])
	}
	if spec != nil {
		only.Content = append(only.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "specs"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{spec}})
	}
	data, err := yaml.Marshal(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{only}})
	if err != nil {
		return InferenceModel{}, err
	}
	return parseModelYAML(data)
}

// validModels keeps the models and specs that validate, alone and together.
// It adds the directory of every model it left something out of to partial.
func validModels(models []packageModel, partial map[string]struct{}) ([]packageModel, []Skipped) {
	whole := FlatCatalog{Models: make([]InferenceModel, len(models))}
	for i := range models {
		whole.Models[i] = models[i].model
	}
	if ValidateFlatCatalog(&whole) == nil {
		return models, nil
	}
	var skipped []Skipped
	skip := func(entry packageModel, spec string, err error) {
		partial[path.Dir(entry.file)] = struct{}{}
		skipped = append(skipped, Skipped{ModelID: strings.ToLower(strings.TrimSpace(entry.model.ModelID)), Spec: strings.TrimSpace(spec), Reason: skipReason(err)})
	}
	validate := func(model InferenceModel) error {
		return validateFlatCatalogStructure(&FlatCatalog{Models: []InferenceModel{model}})
	}
	kept := make([]packageModel, 0, len(models))
	seen := map[string]struct{}{}
	for _, entry := range models {
		if err := validate(entry.model); err != nil {
			alone := entry.model
			alone.Specs = nil
			if modelErr := validate(alone); modelErr != nil {
				skip(entry, "", modelErr)
				continue
			}
			var specs []InferenceSpec
			for _, spec := range entry.model.Specs {
				probe := entry.model
				probe.Specs = []InferenceSpec{spec}
				if specErr := validate(probe); specErr != nil {
					skip(entry, spec.Name, specErr)
					continue
				}
				specs = append(specs, spec)
			}
			remaining := entry.model
			remaining.Specs = specs
			if len(specs) == len(entry.model.Specs) || validate(remaining) != nil {
				// The specs pass alone and fail together.
				skip(entry, "", err)
				continue
			}
			entry.model = remaining
		}
		id := strings.ToLower(strings.TrimSpace(entry.model.ModelID))
		if _, duplicate := seen[id]; duplicate {
			skip(entry, "", fmt.Errorf("duplicate model_id %q", id))
			continue
		}
		seen[id] = struct{}{}
		kept = append(kept, entry)
	}
	// Leaving a model out can strand a spec that runs it as a component, and
	// leaving specs out can strand a model only they referenced.
	for changed := true; changed; {
		changed = false
		present := map[string]struct{}{}
		for _, entry := range kept {
			present[strings.ToLower(strings.TrimSpace(entry.model.ModelID))] = struct{}{}
		}
		referenced := map[string]struct{}{}
		for i := range kept {
			var specs []InferenceSpec
			for _, spec := range kept[i].model.Specs {
				missing := ""
				for _, component := range spec.Components {
					id := strings.ToLower(strings.TrimSpace(component.ModelID))
					if _, ok := present[id]; id != "" && !ok {
						missing = id
						break
					}
				}
				if missing != "" {
					skip(kept[i], spec.Name, fmt.Errorf("component model %s is not available", missing))
					changed = true
					continue
				}
				for _, component := range spec.Components {
					if id := strings.ToLower(strings.TrimSpace(component.ModelID)); id != "" {
						referenced[id] = struct{}{}
					}
				}
				specs = append(specs, spec)
			}
			kept[i].model.Specs = specs
		}
		remaining := kept[:0]
		for _, entry := range kept {
			id := strings.ToLower(strings.TrimSpace(entry.model.ModelID))
			if _, used := referenced[id]; len(entry.model.Specs) == 0 && !used {
				// A model that stated specs is already named by each of them.
				if entry.stated == 0 {
					skip(entry, "", fmt.Errorf("model %s requires at least one spec unless referenced as a runtime component", id))
				}
				changed = true
				continue
			}
			remaining = append(remaining, entry)
		}
		kept = remaining
	}
	return kept, skipped
}
