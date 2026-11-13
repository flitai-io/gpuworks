// Package imagecatalog adapts persisted image rows to the deploy package's
// persistence-neutral resolver. Keeping this bridge outside store avoids
// making the repository layer know how deployment selection works.
package imagecatalog

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func Candidates(st *store.Store) ([]deploy.ImageCandidate, error) {
	if st == nil {
		return nil, nil
	}
	rows, err := st.ListImages()
	if err != nil {
		return nil, err
	}
	out := make([]deploy.ImageCandidate, 0, len(rows))
	for _, row := range rows {
		seriesEnabled := true
		framework := row.Framework
		if series, seriesErr := st.GetImageSeries(row.SeriesID); seriesErr == nil {
			seriesEnabled = series.Enabled
			framework = series.Framework
		}
		out = append(out, deploy.ImageCandidate{
			Engine:           row.Engine,
			EngineVersion:    row.EngineVersion,
			Framework:        framework,
			FrameworkVersion: row.FrameworkVersion,
			Arch:             row.Arch,
			ChipFamily:       accelerator.RunnerImageChipFamily(row.ImageRef),
			ImageRef:         row.ImageRef,
			Source:           row.Source,
			Deprecated:       row.Deprecated,
			Enabled:          row.Enabled && seriesEnabled,
			RollingTag:       row.RollingTag,
			CreatedAt:        row.CreatedAt,
		})
	}
	return out, nil
}

func Resolve(st *store.Store, target deploy.ImageTarget) deploy.ImageResolution {
	candidates, err := Candidates(st)
	if err != nil {
		if accelerator.RequiresVendorCatalogImage(target.Vendor) {
			return deploy.ImageResolution{Source: "catalog", Blocking: true, Warnings: []messages.Warning{messages.NewWarning(messages.WarningImageCatalogUnavailableDomestic, nil)}}
		}
		return deploy.ImageResolution{Image: deploy.ResolveImage(target), Source: "fallback", Warnings: []messages.Warning{messages.NewWarning(messages.WarningImageCatalogUnavailable, nil)}}
	}
	return deploy.ResolveImageCatalog(target, candidates)
}

// EngineOption is one engine value an image catalog entry may declare.
type EngineOption struct {
	Name  string
	Label string
}

// EngineOptions is the image form's engine dictionary, in display order:
// every deploy engine plus the catalog-only vllm-omni family.
func EngineOptions() []EngineOption {
	names := engines.CatalogEngines()
	out := make([]EngineOption, 0, len(names))
	for _, name := range names {
		out = append(out, EngineOption{Name: name, Label: engines.DisplayName(name)})
	}
	return out
}
