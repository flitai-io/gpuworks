package api

import (
	"net/http"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

// benchmarkPriorityClass checks the request priority class a run names: a
// class that exists, on a run whose requests go through the gateway (a
// managed target measured from the server), the only entry that applies
// one. Only an administrator may name one, as only an administrator binds a
// key to any class. It returns the class's name, and writes the refusal
// itself.
func (a *apiHandlers) benchmarkPriorityClass(w http.ResponseWriter, r *http.Request, name, entry string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", true
	}
	if claims, ok := a.claimsFromRequest(r); !ok || claims.Role != "admin" {
		writeProductError(w, http.StatusForbidden, ErrorAdministratorRequired, "only an administrator may choose a request priority class", nil)
		return "", false
	}
	if entry != benchmarkEntryGateway {
		writeProductError(w, http.StatusBadRequest, ErrorBenchmarkPriorityClassGateway, "a request priority class applies only to a run of a managed deployment measured from the server, whose requests go through the gateway", nil)
		return "", false
	}
	table, err := a.d.Store.PriorityClassTable()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return "", false
	}
	if _, ok := table.ByName(name); !ok {
		writeProductError(w, http.StatusBadRequest, messages.ErrorPriorityClassNotFound, "no request priority class is named "+name, nil)
		return "", false
	}
	return name, true
}

// benchmarkObservesPriorityClass says whether the run's answers come from
// this server's gateway, whose X-GPUWorks-Priority-Class names the class a
// request ran at; any other endpoint's header of that name means nothing.
func (a *apiHandlers) benchmarkObservesPriorityClass(run *store.BenchmarkRun) bool {
	return a.managedBenchmarkGateway(run)
}

// benchmarkRequestHeader is what every request of a run sends besides its
// key: the priority class it asks the gateway for, if any, and, only to this
// server's own gateway, that it is this run's, so the gateway records it as an
// evaluation's. An external endpoint is not told the run.
func benchmarkRequestHeader(runID uint, class string, ownGateway bool) http.Header {
	header := http.Header{}
	if ownGateway {
		header.Set(loadgen.BenchmarkRunHeader, strconv.FormatUint(uint64(runID), 10))
	}
	if class != "" {
		header.Set(loadgen.PriorityClassHeader, class)
	}
	if len(header) == 0 {
		return nil
	}
	return header
}
