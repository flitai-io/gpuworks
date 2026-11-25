package manifest

import (
	"errors"
	"fmt"
)

// ErrConflict marks an import refused because a community or custom row
// already owns a catalog series name or image ref.
var ErrConflict = errors.New("catalog conflict")

// SeriesState and ImageState expose what the importer needs to know about an
// existing row: whether it exists, and which source owns it. Which columns of
// an existing system row an import may overwrite is the sink's rule.
type SeriesState struct {
	Exists bool
	Source string
}

type ImageState struct {
	Exists bool
	Source string
}

type SeriesInput struct {
	Name       string
	Engine     string
	Purpose    string
	Framework  string
	Notes      string
	CommonArgs []CommonArg
	CommonEnv  []CommonEnv
}

type ImageInput struct {
	SeriesName       string
	Name             string
	Purpose          string
	Engine           string
	EngineVersion    string
	Framework        string
	FrameworkVersion string
	Variant          string
	Arch             string
	ImageRef         string
	RegistrySources  []string
	RollingTag       bool
	BlobSHA256       string
	SizeBytes        int64
	Deprecated       bool
	DeprecatedSince  string
	Notes            string
}

// Sink is the small persistence adapter shared by every manifest source.
// Implementations may use a database, a test double, or another durable
// catalog; the importer itself remains independent of those details.
type Sink interface {
	FindSeries(name string) (SeriesState, error)
	FindImage(ref string) (ImageState, error)
	// UpsertSeries and UpsertImage create a row or update the catalog-owned
	// columns of an existing system row; operator choices on it survive.
	UpsertSeries(SeriesInput) error
	UpsertImage(ImageInput) error
	// ReconcileMissingOfficial handles every official row whose ref is not in
	// seen: unreferenced rows are deleted, referenced ones deprecated.
	ReconcileMissingOfficial(seen map[string]struct{}, since string) (deleted, deprecated int, err error)
}

// ImportOptions selects how a conflict with a community or custom row is
// handled.
type ImportOptions struct {
	// SkipConflicts records a series name or ref that already belongs to a
	// community/custom row in Result.Skipped and imports the rest (the startup
	// sync must never block the server). When false, any conflict fails the
	// whole import before the first write (an operator-triggered import).
	SkipConflicts bool
}

// Conflict is one manifest entry that was not imported because a community
// or custom row already owns its name or ref.
type Conflict struct {
	Kind   string // "series" or "image"
	Name   string
	Reason string
}

// Result counts what an import changed. Created and Updated count version
// rows; series upserts are not counted.
type Result struct {
	CatalogVersion      string
	Created, Updated    int
	Deleted, Deprecated int
	Skipped             []Conflict
}

// ImportOfficial validates and imports a complete official directory. It
// performs conflict checks before the first write, then removes or deprecates
// the official entries absent from the incoming catalog. Community/custom
// entries are never touched.
func ImportOfficial(m Manifest, sink Sink, opt ImportOptions) (Result, error) {
	result := Result{CatalogVersion: m.CatalogVersion}
	if sink == nil {
		return result, fmt.Errorf("catalog sink is nil")
	}
	if err := m.Validate(); err != nil {
		return result, err
	}
	skipSeries := map[string]bool{}
	skipImage := map[string]bool{}
	for _, series := range m.Series {
		state, err := sink.FindSeries(series.Name)
		if err != nil {
			return result, err
		}
		if state.Exists && state.Source != "" && state.Source != "official" {
			reason := fmt.Sprintf("official catalog series %q conflicts with existing %s series; rename the catalog series", series.Name, state.Source)
			if !opt.SkipConflicts {
				return result, fmt.Errorf("%w: %s", ErrConflict, reason)
			}
			skipSeries[series.Name] = true
			result.Skipped = append(result.Skipped, Conflict{Kind: "series", Name: series.Name, Reason: reason})
			continue
		}
		for _, version := range series.Versions {
			imageState, imageErr := sink.FindImage(version.ImageRef)
			if imageErr != nil {
				return result, imageErr
			}
			if imageState.Exists && imageState.Source != "" && imageState.Source != "official" {
				reason := fmt.Sprintf("official catalog image %q conflicts with existing %s image", version.ImageRef, imageState.Source)
				if !opt.SkipConflicts {
					return result, fmt.Errorf("%w: %s", ErrConflict, reason)
				}
				skipImage[version.ImageRef] = true
				result.Skipped = append(result.Skipped, Conflict{Kind: "image", Name: version.ImageRef, Reason: reason})
			}
		}
	}
	// A skipped entry never enters seen: it is not an official row, so it must
	// not be reconciled as one either way.
	seen := make(map[string]struct{})
	for _, series := range m.Series {
		if skipSeries[series.Name] {
			continue
		}
		if err := sink.UpsertSeries(SeriesInput{
			Name: series.Name, Engine: series.Engine, Purpose: series.Purpose,
			Framework: series.Framework, Notes: series.Notes, CommonArgs: series.CommonArgs, CommonEnv: series.CommonEnv,
		}); err != nil {
			return result, err
		}
		for _, version := range series.Versions {
			if skipImage[version.ImageRef] {
				continue
			}
			imageState, err := sink.FindImage(version.ImageRef)
			if err != nil {
				return result, err
			}
			deprecatedSince := ""
			if version.Deprecated {
				deprecatedSince = version.DeprecatedSince
				if deprecatedSince == "" {
					deprecatedSince = m.CatalogVersion
				}
			}
			name := version.Name
			if name == "" {
				name = series.Name
			}
			if err := sink.UpsertImage(ImageInput{
				SeriesName: series.Name, Name: name, Purpose: series.Purpose,
				Engine: series.Engine, EngineVersion: version.EngineVersion,
				Framework: series.Framework, FrameworkVersion: version.FrameworkVersion,
				Arch: version.Arch, ImageRef: version.ImageRef,
				RegistrySources: version.RegistrySources, BlobSHA256: version.BlobSHA256, RollingTag: version.RollingTag,
				SizeBytes:  version.SizeBytes,
				Deprecated: version.Deprecated, DeprecatedSince: deprecatedSince,
				Notes: version.Notes,
			}); err != nil {
				return result, err
			}
			if imageState.Exists {
				result.Updated++
			} else {
				result.Created++
			}
			seen[version.ImageRef] = struct{}{}
		}
	}
	deleted, deprecated, err := sink.ReconcileMissingOfficial(seen, m.CatalogVersion)
	result.Deleted, result.Deprecated = deleted, deprecated
	return result, err
}
