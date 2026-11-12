package deployment

import (
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func ModelFilePathProductError(err error) error {
	var occupied *store.ModelFilePathOccupiedError
	if !errors.As(err, &occupied) {
		return err
	}
	return productError(messages.ErrorModelFilePathOccupied,
		fmt.Sprintf("Path %q in storage #%d is already used by model #%d. Choose another storage.", occupied.Path, occupied.StorageID, occupied.ModelID),
		map[string]any{"storage_id": occupied.StorageID, "path": occupied.Path, "model_id": occupied.ModelID})
}
