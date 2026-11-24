package store

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

var ErrLocalImageCatalogConflict = errors.New("image catalog changed during node registration")

// BindCatalogSeries applies the series' framework before checking the version
// used for automatic selection. A framework version must be explicitly declared.
func (img *Image) BindCatalogSeries(series *ImageSeries) error {
	framework := series.Framework
	if framework == "" {
		framework = img.Framework
	}
	if strings.TrimSpace(framework) != "" && strings.TrimSpace(img.FrameworkVersion) == "" {
		return messages.NewProductError(messages.ErrorImageFrameworkVersionRequired, "framework_version is required when a framework is declared", map[string]any{"image": img.ImageRef, "framework": framework})
	}
	img.SeriesID = series.ID
	img.Framework = framework
	return nil
}

// CheckLocalImageRegistration checks the effective series before the node copies any
// layers. The returned version ID detects deletion or replacement during that copy.
func (s *Store) CheckLocalImageRegistration(series ImageSeries, version Image) (uint, error) {
	current, err := s.ImageByRef(version.ImageRef)
	if err == nil {
		if current.Source != "custom" {
			return 0, ErrLocalImageCatalogConflict
		}
		return current.ID, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	existing, err := s.ImageSeriesByName(series.Name)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return 0, bindLocalSeries(&version, existing)
}

func bindLocalSeries(version *Image, series *ImageSeries) error {
	if series.Source != "custom" || (series.Framework != "" && version.Framework != "" && !strings.EqualFold(series.Framework, version.Framework)) {
		return fmt.Errorf("%w: incompatible series %q", ErrLocalImageCatalogConflict, series.Name)
	}
	return version.BindCatalogSeries(series)
}

// RegisterLocalImage creates only missing catalog rows. The final ref lookup,
// optional insertion and confirmed presence share one transaction. A background
// copy registers only the catalog version until its worker confirms presence.
func (s *Store) RegisterLocalImage(nodeID, expectedID uint, defaults ImageSeries, version Image, observation *proto.ImagePresence) (*ImageSeries, uint, error) {
	var series ImageSeries
	var image Image
	var createdSeries, createdVersion bool
	err := s.db.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("image_ref = ?", version.ImageRef).First(&image).Error
		switch {
		case err == nil:
			if image.Source != "custom" || (expectedID != 0 && expectedID != image.ID) {
				return ErrLocalImageCatalogConflict
			}
			if err := tx.First(&series, image.SeriesID).Error; err != nil {
				return err
			}
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		case expectedID != 0:
			return ErrLocalImageCatalogConflict
		default:
			result := tx.Where("name = ?", defaults.Name).Attrs(defaults).FirstOrCreate(&series)
			if result.Error != nil {
				return result.Error
			}
			createdSeries = result.RowsAffected != 0
			if err := bindLocalSeries(&version, &series); err != nil {
				return err
			}
			if err := tx.Create(&version).Error; err != nil {
				return err
			}
			createdVersion = true
			image = version
		}
		if observation == nil {
			return nil
		}
		observation.ImageRef = image.ImageRef
		return reconcileImagePresence(tx, nodeID, []proto.ImagePresence{*observation})
	})
	if err != nil {
		return nil, 0, err
	}
	if createdSeries {
		s.Bus.PublishChange("image", series.Name, "series_created")
	}
	if createdVersion {
		s.Bus.PublishChange("image", image.ImageRef, "created")
	}
	if observation != nil {
		s.Bus.PublishChange("image", uintKey(image.ID), "node")
	}
	return &series, image.ID, nil
}

// A delayed copy report for a deleted catalog version cannot attach to a new
// version created under the same tag, even if inventory already added its row.
func runtimeImageReportMatches(row ImageNode, st proto.ImageNodeStatus) bool {
	return st.Source != proto.RuntimeImageCopySource || (row.Source == st.Source && st.TransferID != "" && (row.TransferID == st.TransferID || row.LastTransferID == st.TransferID))
}
