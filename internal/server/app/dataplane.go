package app

import (
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/proxy"
)

// dataPlane is the one handler behind /v1: the model list, the key's
// information, the OpenAI compatible files and batches, and every other
// path, which is a request for a model. Each of them authenticates by API
// key itself.
func dataPlane(models, key, batches, gateway http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/v1/models":
			models.ServeHTTP(w, r)
		case path == proxy.KeyPath:
			key.ServeHTTP(w, r)
		case isBatchPath(path):
			batches.ServeHTTP(w, r)
		default:
			gateway.ServeHTTP(w, r)
		}
	})
}

func isBatchPath(path string) bool {
	for _, root := range []string{"/v1/files", "/v1/batches"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}
