package inferencecatalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
)

// LoadPackage reads a catalog rooted at fsys. It rejects files outside the
// model-owned layout so an embedded, downloaded, and offline-imported package
// all have the same authority boundary.
func LoadPackage(fsys fs.FS) (Package, error) {
	pkg, _, err := loadPackage(fsys, false)
	return pkg, err
}

// packageModel is a model of a package being loaded and the file it came
// from.
type packageModel struct {
	model InferenceModel
	file  string
	// stated is the number of specs the file states, read or not.
	stated int
}

// loadPackage is LoadPackage and LoadPackageTolerant. A tolerant load leaves
// out the models and specs this build cannot read or validate and names them
// in skipped; everything else about the package is checked alike.
func loadPackage(fsys fs.FS, tolerant bool) (Package, []Skipped, error) {
	manifestData, err := fs.ReadFile(fsys, "manifest.yaml")
	if err != nil {
		return Package{}, nil, fmt.Errorf("read inference catalog package manifest: %w", err)
	}
	manifest, err := parsePackageManifest(manifestData, !tolerant)
	if err != nil {
		return Package{}, nil, err
	}

	var modelFiles []string
	err = fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." || entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("inference catalog package contains non-regular file %s", name)
		}
		if err := validatePackageFileName(name); err != nil {
			return err
		}
		if path.Base(name) == "model.yaml" {
			modelFiles = append(modelFiles, name)
		}
		return nil
	})
	if err != nil {
		return Package{}, nil, fmt.Errorf("walk inference catalog package: %w", err)
	}
	if len(modelFiles) == 0 {
		return Package{}, nil, errors.New("inference catalog package requires at least one models/**/model.yaml")
	}
	sort.Strings(modelFiles)

	// The manifest counts what the package states, whether or not this build
	// can read all of it.
	statedSpecs := 0
	var skipped []Skipped
	// partial names the model directories a skip left something out of.
	partial := map[string]struct{}{}
	models := make([]packageModel, 0, len(modelFiles))
	for _, name := range modelFiles {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return Package{}, nil, fmt.Errorf("read inference catalog model %s: %w", name, err)
		}
		model, err := parseModelYAML(data)
		stated := len(model.Specs)
		if err != nil {
			if !tolerant {
				return Package{}, nil, fmt.Errorf("parse inference catalog model %s: %w", name, err)
			}
			readable, specs, left, ok := parseModelTolerant(name, data)
			if !ok {
				return Package{}, nil, fmt.Errorf("parse inference catalog model %s: %w", name, err)
			}
			stated = specs
			statedSpecs += specs
			skipped = append(skipped, left...)
			partial[path.Dir(name)] = struct{}{}
			if readable == nil {
				continue
			}
			model = *readable
		} else {
			statedSpecs += stated
		}
		modelDirectory := path.Dir(name)
		if _, err := fs.Stat(fsys, modelDirectory+"/README.zh-CN.md"); err == nil {
			model.ReadmeZHCN = "README.zh-CN.md"
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Package{}, nil, fmt.Errorf("inspect model %s Chinese README: %w", model.ModelID, err)
		}
		if _, err := fs.Stat(fsys, modelDirectory+"/README.en.md"); err == nil {
			model.ReadmeEN = "README.en.md"
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Package{}, nil, fmt.Errorf("inspect model %s English README: %w", model.ModelID, err)
		}
		models = append(models, packageModel{model: model, file: name, stated: stated})
	}
	if tolerant {
		var left []Skipped
		models, left = validModels(models, partial)
		skipped = append(skipped, left...)
		if len(models) == 0 {
			return Package{}, nil, errors.New("inference catalog package has no model this server can read")
		}
	}
	catalog := FlatCatalog{Models: make([]InferenceModel, len(models))}
	for i := range models {
		catalog.Models[i] = models[i].model
	}
	if err := ValidateFlatCatalog(&catalog); err != nil {
		return Package{}, nil, err
	}
	for i := range models {
		expected, err := modelFileName(catalog.Models[i].ModelID)
		if err != nil {
			return Package{}, nil, err
		}
		if models[i].file != expected {
			return Package{}, nil, fmt.Errorf("model %s must be stored at %s, found %s", catalog.Models[i].ModelID, expected, models[i].file)
		}
	}
	if err := validatePackageAssets(fsys, &catalog, partial); err != nil {
		return Package{}, nil, err
	}
	// Filesystem ordering includes the trailing "/model.yaml", so it is not
	// equivalent to ordering bare model IDs (for example "foo-2" sorts before
	// "foo/model.yaml"). Keep the public catalog order independent of layout.
	sort.Slice(catalog.Models, func(i, j int) bool {
		return catalog.Models[i].ModelID < catalog.Models[j].ModelID
	})

	if manifest.Models != len(modelFiles) || manifest.Specs != statedSpecs {
		return Package{}, nil, fmt.Errorf("inference catalog manifest says %d models/%d specs, package contains %d/%d", manifest.Models, manifest.Specs, len(modelFiles), statedSpecs)
	}
	return Package{Manifest: manifest, Catalog: catalog}, skipped, nil
}

func LoadPackageDirectory(root string) (Package, error) {
	return LoadPackage(os.DirFS(root))
}
