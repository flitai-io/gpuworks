package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) deleteBenchmark(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	run, err := a.d.Store.GetBenchmarkRun(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	points, err := a.d.Store.ListBenchmarkPoints(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.d.Store.DeleteBenchmarkRun(id); errors.Is(err, store.ErrBenchmarkActive) {
		writeErr(w, http.StatusConflict, err)
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	root := filepath.Clean(a.d.BenchmarkArtifactDir)
	if root != "." {
		for _, point := range points {
			clean := filepath.Clean(point.DetailPath)
			if clean != "." && strings.HasPrefix(clean, root+string(os.PathSeparator)) {
				_ = os.Remove(clean)
			}
		}
	}
	if run.LoadOrigin != "server" && a.d.NodeSync != nil {
		_ = a.d.NodeSync.SyncNode(run.LoadOrigin)
	}
	w.WriteHeader(http.StatusNoContent)
}
