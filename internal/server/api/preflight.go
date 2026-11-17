package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// preflightPairCheck is the check ID of one directed pair (ADR 0067); the
// pair's source_uuid and target_uuid say which.
const preflightPairCheck = "node_pair"

// preflightPair is one directed probe, reported as a check result: Status is
// proto.CheckPass or CheckFail, Code and Params say what was found, Message is
// the English fallback. Evidence is the agent's or the RPC transport's own
// text (external output), kept apart from the message; BandwidthEvidence is
// the agent's text about the bandwidth measurement.
type preflightPair struct {
	ID                string              `json:"id"`
	Status            string              `json:"status"`
	Code              string              `json:"code"`
	Params            preflightPairParams `json:"params"`
	Message           string              `json:"message,omitempty"`
	SourceUUID        string              `json:"source_uuid"`
	TargetUUID        string              `json:"target_uuid"`
	Target            string              `json:"target"`
	Reachable         bool                `json:"reachable"`
	RTTP50MS          float64             `json:"rtt_ms_p50"`
	RTTP99MS          float64             `json:"rtt_ms_p99"`
	Gbps              float64             `json:"gbps,omitempty"`
	BandwidthTested   bool                `json:"bandwidth_tested"`
	BytesSent         int64               `json:"bytes_sent,omitempty"`
	BytesReceived     int64               `json:"bytes_received,omitempty"`
	DurationSeconds   float64             `json:"duration_seconds,omitempty"`
	Evidence          string              `json:"evidence,omitempty"`
	BandwidthEvidence string              `json:"bandwidth_evidence,omitempty"`
	// failure is how the pair failed before its probe answered (the target's
	// listener did not open, or the probe call itself failed); empty when the
	// probe answered.
	failure string
}

// preflightPairParams are the interpolation parameters of a pair's code:
// the source and target node UUIDs, the probed address, and for a listener
// failure the port the target was asked to open.
type preflightPairParams struct {
	Address string `json:"address"`
	Port    int    `json:"port,omitempty"`
	Source  string `json:"source"`
	Target  string `json:"target"`
}

// distributedPreflightResponse: OK is true when no pair failed.
type distributedPreflightResponse struct {
	OK    bool            `json:"ok"`
	Pairs []preflightPair `json:"pairs"`
}

type preflightListener struct {
	Token  string
	Target string
}

func (a *apiHandlers) distributedPreflight(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	var req deployment.DistributedPreflightRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := deployment.ValidatePreflightRequest(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	hosts := make(map[string]string, len(req.NodeUUIDs))
	nodes := make([]*store.Node, 0, len(req.NodeUUIDs))
	for _, uuid := range req.NodeUUIDs {
		node, err := a.d.Store.NodeWithGPUs(uuid)
		if err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorPreflightNodeInvalid, fmt.Sprintf("node %s: %v", uuid, err), map[string]any{"node": uuid, "detail": err.Error()})
			return
		}
		if a.d.Online != nil && !a.d.Online(uuid) {
			writeProductError(w, http.StatusConflict, ErrorPreflightNodeOffline, fmt.Sprintf("node %s is offline", uuid), map[string]any{"node": uuid})
			return
		}
		nodes = append(nodes, node)
	}
	// The ranks probe each other on the network the group would share.
	group, err := a.deployments().ComputeGroup(nodes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	for _, node := range nodes {
		hosts[node.UUID] = group.Ranks[node.ID].IP
	}
	if req.MasterAddr != "" {
		host, err := deployment.MasterHost(req.MasterAddr)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		hosts[req.NodeUUIDs[0]] = host
	}

	out := distributedPreflightResponse{OK: true, Pairs: make([]preflightPair, 0, len(req.NodeUUIDs)*(len(req.NodeUUIDs)-1))}
	listeners := make(map[string]preflightListener, len(req.NodeUUIDs))
	listenerErrors := make(map[string]string) // the external text of each listener failure
	ttl := preflightListenerTTL(req)
	for _, targetUUID := range req.NodeUUIDs {
		token, err := randomNetBenchToken()
		if err != nil {
			listenerErrors[targetUUID] = "generate listener token: " + err.Error()
			out.OK = false
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		raw, callErr := a.d.CallNode(ctx, targetUUID, proto.RPCNetBenchStart, map[string]any{
			"port": req.Port, "token": token, "ttl_seconds": ttl,
		})
		cancel()
		if callErr != nil {
			a.stopPreflightListeners(map[string]preflightListener{targetUUID: {Token: token}})
			listenerErrors[targetUUID] = callErr.Error()
			out.OK = false
			continue
		}
		var started struct {
			Port int `json:"port"`
		}
		if err := json.Unmarshal(raw, &started); err != nil || started.Port != req.Port {
			a.stopPreflightListeners(map[string]preflightListener{targetUUID: {Token: token}})
			if err != nil {
				listenerErrors[targetUUID] = "decode listener response: " + err.Error()
			} else {
				listenerErrors[targetUUID] = fmt.Sprintf("listener opened unexpected port %d", started.Port)
			}
			out.OK = false
			continue
		}
		listeners[targetUUID] = preflightListener{Token: token, Target: net.JoinHostPort(hosts[targetUUID], strconv.Itoa(started.Port))}
	}
	defer a.stopPreflightListeners(listeners)

	for _, sourceUUID := range req.NodeUUIDs {
		for _, targetUUID := range req.NodeUUIDs {
			if sourceUUID == targetUUID {
				continue
			}
			listener, started := listeners[targetUUID]
			pair := preflightPair{SourceUUID: sourceUUID, TargetUUID: targetUUID}
			if started {
				pair.Target = listener.Target
			} else {
				pair.Target, _ = probeTarget(hosts[targetUUID], req.Port)
				pair.failure = messages.CheckPreflightListenerFailed
				pair.Evidence = listenerErrors[targetUUID]
				out.addPair(pair, req.Bandwidth, req.Port)
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.Seconds+15)*time.Second)
			raw, callErr := a.d.CallNode(ctx, sourceUUID, proto.RPCNetProbe, map[string]any{
				"target": listener.Target, "token": listener.Token, "bandwidth": req.Bandwidth, "seconds": req.Seconds,
			})
			cancel()
			if callErr != nil {
				pair.failure = messages.CheckPreflightProbeFailed
				pair.Evidence = callErr.Error()
				out.addPair(pair, req.Bandwidth, req.Port)
				continue
			}
			var probe struct {
				Reachable       bool    `json:"reachable"`
				P50             float64 `json:"rtt_ms_p50"`
				P99             float64 `json:"rtt_ms_p99"`
				Gbps            float64 `json:"gbps"`
				BandwidthTested bool    `json:"bandwidth_tested"`
				BytesSent       int64   `json:"bytes_sent"`
				BytesReceived   int64   `json:"bytes_received"`
				DurationSeconds float64 `json:"duration_seconds"`
				Error           string  `json:"error"`
				BandwidthError  string  `json:"bandwidth_error"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				pair.failure = messages.CheckPreflightProbeFailed
				pair.Evidence = "decode probe response: " + err.Error()
			} else {
				pair.Reachable = probe.Reachable
				pair.RTTP50MS = probe.P50
				pair.RTTP99MS = probe.P99
				pair.Gbps = probe.Gbps
				pair.BandwidthTested = probe.BandwidthTested
				pair.BytesSent = probe.BytesSent
				pair.BytesReceived = probe.BytesReceived
				pair.DurationSeconds = probe.DurationSeconds
				pair.Evidence = probe.Error
				pair.BandwidthEvidence = probe.BandwidthError
			}
			out.addPair(pair, req.Bandwidth, req.Port)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func preflightListenerTTL(req deployment.DistributedPreflightRequest) int {
	ttl := len(req.NodeUUIDs)*(len(req.NodeUUIDs)-1)*(req.Seconds+5) + 30
	if ttl < 60 {
		return 60
	}
	if ttl > 3600 {
		return 3600
	}
	return ttl
}

func randomNetBenchToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func (a *apiHandlers) stopPreflightListeners(listeners map[string]preflightListener) {
	for nodeUUID, listener := range listeners {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = a.d.CallNode(ctx, nodeUUID, proto.RPCNetBenchStop, map[string]string{"token": listener.Token})
		cancel()
	}
}

// addPair judges one directed pair and records it. A probe that answered
// fails when the target was unreachable, the agent reported an error, or
// requested bandwidth was not measured.
func (out *distributedPreflightResponse) addPair(pair preflightPair, bandwidthRequested bool, port int) {
	pair.ID = preflightPairCheck
	pair.Status = proto.CheckFail
	pair.Params = preflightPairParams{Source: pair.SourceUUID, Target: pair.TargetUUID, Address: pair.Target}
	switch {
	case pair.failure == messages.CheckPreflightListenerFailed:
		pair.Code, pair.Message = pair.failure, fmt.Sprintf("node %s did not open the probe listener on port %d", pair.TargetUUID, port)
		pair.Params.Port = port
	case pair.failure != "":
		pair.Code, pair.Message = pair.failure, fmt.Sprintf("node %s did not run the probe to node %s", pair.SourceUUID, pair.TargetUUID)
	case !pair.Reachable || pair.Evidence != "":
		pair.Code, pair.Message = messages.CheckPreflightPairUnreachable, fmt.Sprintf("node %s could not reach node %s at %s", pair.SourceUUID, pair.TargetUUID, pair.Target)
	case bandwidthRequested && (!pair.BandwidthTested || pair.BandwidthEvidence != "" || pair.Gbps <= 0):
		pair.Code, pair.Message = messages.CheckPreflightBandwidthNotMeasured, fmt.Sprintf("bandwidth from node %s to node %s was not measured", pair.SourceUUID, pair.TargetUUID)
	default:
		pair.Status = proto.CheckPass
		pair.Code, pair.Message = messages.CheckPreflightPairReachable, fmt.Sprintf("node %s reached node %s at %s", pair.SourceUUID, pair.TargetUUID, pair.Target)
	}
	if pair.Status == proto.CheckFail {
		out.OK = false
	}
	out.Pairs = append(out.Pairs, pair)
}

func probeTarget(addr string, port int) (string, error) {
	host, err := planning.PeerHost(addr)
	if err != nil {
		return "", err
	}
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("target probe port is required")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
