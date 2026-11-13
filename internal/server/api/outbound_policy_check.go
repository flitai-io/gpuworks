package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
)

func (a *apiHandlers) checkServerOutboundPolicy(w http.ResponseWriter, r *http.Request) {
	var request dto.ServerOutboundPolicyCheck
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&request)
	if err == nil {
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			err = errors.New("trailing JSON")
		}
	}
	if err != nil || request.Hosts == nil || len(request.Hosts) > 32 {
		writeProductError(w, http.StatusBadRequest, ErrorServerOutboundPolicyInvalid, "provide at most 32 exact source hosts", nil)
		return
	}
	hosts := make([]string, 0, len(request.Hosts))
	for _, raw := range request.Hosts {
		host, err := outbound.NormalizeHost(raw)
		if err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorServerOutboundPolicyInvalid, "provide exact source hosts without URLs, ports, CIDRs or wildcards", nil)
			return
		}
		hosts = append(hosts, host)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	transport := a.outboundTransport()
	defer transport.CloseIdleConnections()
	results := make([]dto.ServerOutboundHostCheck, 0, len(hosts))
	for _, host := range hosts {
		status := "allowed"
		switch err := transport.CheckHost(ctx, host); {
		case errors.Is(err, outbound.ErrTargetNeedsAllow):
			status = "needs_allow"
		case errors.Is(err, outbound.ErrTargetDenied):
			status = "blocked"
		case err != nil:
			status = "unresolved"
		}
		results = append(results, dto.ServerOutboundHostCheck{Host: host, Status: status})
	}
	writeJSON(w, http.StatusOK, dto.NewList(results))
}
