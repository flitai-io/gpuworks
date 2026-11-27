package app

import (
	"bytes"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/offlinepkg"
)

// withDL serves embedded (or override-dir) agent binaries and the offline
// package built from them at /dl/ in front of the SPA handler.
func withDL(dl, offline, next http.Handler, pinned ...func(string) ([]byte, bool, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offlinepkg.Handles(r.URL.Path) {
			offline.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			if len(pinned) > 0 && pinned[0] != nil {
				binary, handled, err := pinned[0](strings.TrimPrefix(r.URL.Path, "/dl/"))
				if err != nil {
					http.Error(w, "agent upgrade download is blocked; check the Agent upgrade task", http.StatusConflict)
					return
				}
				if handled {
					w.Header().Set("Content-Type", "application/octet-stream")
					http.ServeContent(w, r, strings.TrimPrefix(r.URL.Path, "/dl/"), time.Time{}, bytes.NewReader(binary))
					return
				}
			}
			dl.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
