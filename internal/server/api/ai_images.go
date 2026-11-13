package api

import (
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// validateAskImages applies the image limits to an ask: each message's images
// (aihelper.ValidateImages), images on user turns only, and the request's
// total. The answer names the rule and where it was broken, never the image.
func validateAskImages(w http.ResponseWriter, body dto.AIAskRequest) bool {
	total := len(body.Images)
	check := func(images []string, where string) bool {
		err := aihelper.ValidateImages(images)
		var ie *aihelper.ImageError
		if errors.As(err, &ie) {
			writeProductError(w, http.StatusBadRequest, ErrorAIImageInvalid, "an image of the "+where+" is not usable: "+ie.Code,
				map[string]any{"reason": ie.Code, "where": where, "index": ie.Index, "limit": aihelper.ImagesPerMessage, "max_bytes": aihelper.ImageMaxBytes})
			return false
		}
		return true
	}
	if !check(body.Images, "question") {
		return false
	}
	for _, m := range body.History {
		if len(m.Images) == 0 {
			continue
		}
		if m.Role != "user" {
			writeProductError(w, http.StatusBadRequest, ErrorAIImageInvalid, "only user turns carry images",
				map[string]any{"reason": "role", "where": "history", "index": 0, "limit": aihelper.ImagesPerMessage, "max_bytes": aihelper.ImageMaxBytes})
			return false
		}
		if !check(m.Images, "history") {
			return false
		}
		total += len(m.Images)
	}
	if total > aihelper.ImagesPerRequest {
		writeProductError(w, http.StatusBadRequest, ErrorAIImageInvalid, "too many images in one ask",
			map[string]any{"reason": "too_many", "where": "request", "index": aihelper.ImagesPerRequest, "limit": aihelper.ImagesPerRequest, "max_bytes": aihelper.ImageMaxBytes})
		return false
	}
	return true
}

func historyImages(history []aihelper.Message) int {
	n := 0
	for _, m := range history {
		n += len(m.Images)
	}
	return n
}

// writeAskErr answers what failed before an answer started, and reports
// whether there was nothing to answer: a missing subject is a 404, images to
// a service not marked as taking them a 409 the console can explain, images
// the service refused a 422, and anything else the service failing.
func writeAskErr(w http.ResponseWriter, err error, req aihelper.AskRequest) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, aihelper.ErrImageInputOff):
		writeProductError(w, http.StatusConflict, ErrorAIImageInputOff, "the AI service in use is not marked as accepting images", nil)
	case (len(req.Images) > 0 || historyImages(req.History) > 0) && aihelper.ImageRefused(err):
		writeProductError(w, http.StatusUnprocessableEntity, ErrorAIImageRejected, "the AI service refused the images", nil)
	default:
		writeErr(w, http.StatusBadGateway, err)
	}
	return false
}
