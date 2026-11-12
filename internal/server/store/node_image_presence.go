package store

import (
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gpuworks.ai/gpuworks/internal/proto"
)

func reconcileImagePresence(tx *gorm.DB, nodeID uint, reports []proto.ImagePresence) error {
	// One read of the reported images and one of the node's rows for them:
	// every agent reports every catalog image once a minute, and reading
	// row by row held the store's writer for most of a fleet's status time.
	refs := make([]string, 0, len(reports))
	for _, report := range reports {
		if ref := strings.TrimSpace(report.ImageRef); ref != "" {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	var images []Image
	if err := tx.Where("image_ref IN ?", refs).Order("id asc").Find(&images).Error; err != nil {
		return err
	}
	byRef := make(map[string]Image, len(images))
	imageIDs := make([]uint, 0, len(images))
	for _, image := range images {
		if _, seen := byRef[image.ImageRef]; !seen { // First() by ref took the lowest id
			byRef[image.ImageRef] = image
			imageIDs = append(imageIDs, image.ID)
		}
	}
	if len(imageIDs) == 0 {
		return nil // the catalog may not have been synchronized yet
	}
	var existing []ImageNode
	if err := tx.Where("node_id = ? AND image_version_id IN ?", nodeID, imageIDs).Order("id asc").Find(&existing).Error; err != nil {
		return err
	}
	rows := make(map[uint]ImageNode, len(existing))
	for _, row := range existing {
		if _, seen := rows[row.ImageVersionID]; !seen {
			rows[row.ImageVersionID] = row
		}
	}
	for _, report := range reports {
		image, ok := byRef[strings.TrimSpace(report.ImageRef)]
		if !ok {
			continue // the catalog may not have been synchronized yet
		}
		state := ImageStateAbsent
		if report.Present {
			state = ImageStatePresent
		}
		row, ok := rows[image.ID]
		if !ok {
			// A sync or pull start can create the same row between the read
			// above and this insert; ON CONFLICT DO NOTHING concedes to it, and
			// the re-read below reconciles against whichever row actually won.
			digest := ""
			if report.Present {
				digest = strings.TrimSpace(report.ImageID)
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ImageNode{ImageVersionID: image.ID, ImageID: image.ID, NodeID: nodeID, State: state, SizeBytes: report.SizeBytes, ImageDigestID: digest}).Error; err != nil {
				return err
			}
			if err := tx.Where("image_version_id = ? AND node_id = ?", image.ID, nodeID).First(&row).Error; err != nil {
				return err
			}
		}
		// An inventory snapshot reports runtime presence, not transfer outcome.
		// Preserve an active or failed transfer until an explicit retry/update;
		// otherwise a snapshot racing the terminal result can hide the failure.
		if inImageTransfer(row.State) || (!report.Present && row.State == ImageStateFailed) {
			state = row.State
		}
		runtimeSize := report.RuntimeSizeBytes
		if !report.Present || inImageTransfer(row.State) || runtimeSize != nil && *runtimeSize < 0 {
			runtimeSize = nil
		}
		runtimeSizeChanged := (runtimeSize == nil) != (row.RuntimeSizeBytes == nil) || runtimeSize != nil && row.RuntimeSizeBytes != nil && *runtimeSize != *row.RuntimeSizeBytes
		sizeChanged := report.SizeBytes > 0 && report.SizeBytes != row.SizeBytes
		// A transfer still owns the row, so its digest stays until that
		// transfer reports. A present report with no id did not observe one
		// and must not wipe a digest already stored. Absence did observe that
		// the image is gone, so the old identity goes with it.
		imageID := strings.TrimSpace(report.ImageID)
		digest, digestChanged := row.ImageDigestID, false
		if !report.Present {
			digest, digestChanged = "", row.ImageDigestID != ""
		} else if !inImageTransfer(row.State) && imageID != "" && imageID != row.ImageDigestID {
			digest, digestChanged = imageID, true
		}
		// A settled row the report leaves as it is needs no write. A transfer
		// in flight still takes one: its updated_at is how the stall watch
		// (hub image_direct_watch.go) tells a live transfer from a dead one.
		inFlight := row.State == ImageStateSyncing || row.State == ImageStatePulling
		if state == row.State && !sizeChanged && !runtimeSizeChanged && !digestChanged && !inFlight {
			continue
		}
		updates := map[string]any{"state": state, "runtime_size_bytes": runtimeSize}
		if row.State == ImageStateFailed && state == ImageStatePresent {
			// The failed transfer's source and reason describe an attempt that
			// did not bring the image: it arrived another way, typically a
			// replica pulling it itself after a copy from a peer failed.
			updates["source"], updates["source_node_id"], updates["message"], updates["transferred_bytes"] = "", uint(0), "", int64(0)
		}
		if report.SizeBytes > 0 {
			updates["size_bytes"] = report.SizeBytes
		}
		if digestChanged {
			updates["image_digest_id"] = digest
		}
		if err := tx.Model(&ImageNode{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
			return err
		}
		row.State = state
		row.RuntimeSizeBytes = runtimeSize
		if report.SizeBytes > 0 {
			row.SizeBytes = report.SizeBytes
		}
		if digestChanged {
			row.ImageDigestID = digest
		}
		rows[image.ID] = row
	}
	return nil
}

func inImageTransfer(state string) bool {
	return state == ImageStateSyncing || state == ImageStatePulling
}
