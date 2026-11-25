package imagecatalog

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// manifestSink writes a system catalog import into the store. Which columns of
// an existing system row the catalog may overwrite is the store's rule
// (UpsertImage / UpsertImageSeries), so the sink passes catalog data through.
type manifestSink struct{ store *store.Store }

func (s manifestSink) FindSeries(name string) (manifest.SeriesState, error) {
	row, err := s.store.ImageSeriesByName(name)
	if err == store.ErrNotFound {
		return manifest.SeriesState{}, nil
	}
	if err != nil {
		return manifest.SeriesState{}, err
	}
	return manifest.SeriesState{Exists: true, Source: row.Source}, nil
}

func (s manifestSink) FindImage(ref string) (manifest.ImageState, error) {
	row, err := s.store.ImageByRef(ref)
	if err == store.ErrNotFound {
		return manifest.ImageState{}, nil
	}
	if err != nil {
		return manifest.ImageState{}, err
	}
	return manifest.ImageState{Exists: true, Source: row.Source}, nil
}

func (s manifestSink) UpsertSeries(in manifest.SeriesInput) error {
	commonArgs, _ := json.Marshal(in.CommonArgs)
	commonEnv, _ := json.Marshal(in.CommonEnv)
	_, err := s.store.UpsertImageSeries(&store.ImageSeries{Name: in.Name, Engine: in.Engine, Purpose: in.Purpose, Framework: in.Framework, CommonArgs: string(commonArgs), CommonEnv: string(commonEnv), Source: "official", Notes: in.Notes, Enabled: true})
	return err
}

func (s manifestSink) UpsertImage(in manifest.ImageInput) error {
	series, err := s.store.ImageSeriesByName(in.SeriesName)
	if err != nil {
		return err
	}
	sources, _ := json.Marshal(in.RegistrySources)
	_, err = s.store.UpsertImage(&store.Image{SeriesID: series.ID, Name: in.Name, Purpose: in.Purpose, Engine: in.Engine, EngineVersion: in.EngineVersion, Framework: in.Framework, FrameworkVersion: in.FrameworkVersion, Arch: in.Arch, ImageRef: in.ImageRef, Source: "official", RegistrySources: string(sources), OfflineBlobSHA256: in.BlobSHA256, RollingTag: in.RollingTag, SizeBytes: in.SizeBytes, Deprecated: in.Deprecated, DeprecatedSince: in.DeprecatedSince, Notes: in.Notes, Enabled: true})
	return err
}

func (s manifestSink) ReconcileMissingOfficial(seen map[string]struct{}, since string) (int, int, error) {
	return s.store.ReconcileMissingOfficialImages(seen, since)
}
