package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestImageLocalImportAndRetryKeepNodeFileSource(t *testing.T) {
	st, image := imageWithSources(t, "file:api_local_import?mode=memory&cache=shared", nil)
	for _, retry := range []bool{false, true} {
		rec := &pullRecorder{}
		h := apiHandlers{d: rec.deps(st)}
		route := h.syncImageToNode
		if retry {
			route = h.retryImageToNode
		}
		resp := postJSON(t, route, "/", `{"source":" local ","archive_path":" /data/image with spaces.tar "}`, syncRoute(image, "node-1"))
		if resp.Code != http.StatusAccepted || len(rec.pulls) != 1 || len(rec.peers) != 0 || len(rec.retries) != 0 {
			t.Fatalf("local request: %d %s, calls = %+v", resp.Code, resp.Body.String(), rec)
		}
		got := rec.pulls[0]
		if !got.IsLocalFetch() || got.ArchivePath != "/data/image with spaces.tar" || got.IsURLFetch() || got.Force {
			t.Fatalf("local import changed into another acquisition: %+v", got)
		}
	}
}

func TestImageLocalImportRejectsAmbiguousSources(t *testing.T) {
	st, image := imageWithSources(t, "file:api_local_invalid?mode=memory&cache=shared", nil)
	for _, body := range []string{
		`{"source":"local"}`,
		`{"source":"local","archive_path":"relative.tar"}`,
		`{"archive_path":"/data/image.tar"}`,
		`{"source":"url","archive_path":"/data/image.tar"}`,
		`{"source":"local","archive_path":"/data/image.tar","source_node":"peer"}`,
		`{"source":"local","archive_path":"/data/image.tar","archive_url":"https://mirror/image.tar"}`,
		`{"source":"local","archive_path":"/data/image.tar","relay":true}`,
	} {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retry=%v", body, retry), func(t *testing.T) {
				rec := &pullRecorder{}
				h := apiHandlers{d: rec.deps(st)}
				route := h.syncImageToNode
				if retry {
					route = h.retryImageToNode
				}
				resp := postJSON(t, route, "/", body, syncRoute(image, "node-1"))
				var errorBody map[string]any
				if err := json.Unmarshal(resp.Body.Bytes(), &errorBody); err != nil || errorBody["code"] != "IMAGE_ARCHIVE_PATH_INVALID" {
					t.Fatalf("missing stable error code: %s", resp.Body.String())
				}
				if resp.Code != http.StatusBadRequest || len(rec.pulls)+len(rec.peers)+len(rec.retries) != 0 {
					t.Fatalf("invalid input dispatched: %d %s, %+v", resp.Code, resp.Body.String(), rec)
				}
			})
		}
	}
}
