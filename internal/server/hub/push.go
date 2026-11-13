package hub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SetEnginePorts gives the hub the controller's engine port range, which each
// state sync carries so agents report the host listeners inside it.
func (h *Hub) SetEnginePorts(ports func() proto.PortRange) { h.enginePorts = ports }

// PullImage asks a connected agent to pull a catalog image from the
// candidates its pull source rules give it and tag it back to the canonical
// catalog ref. An agent that walks candidates itself gets them all in one
// call; an older one gets them one by one. The agent reports terminal state
// asynchronously through status.image_node.
func (h *Hub) PullImage(ctx context.Context, imageID uint, targetNodeUUID string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
	image, err := h.store.GetImage(imageID)
	if err != nil {
		return nil, err
	}
	target, err := h.store.NodeByUUID(targetNodeUUID)
	if err != nil {
		return nil, err
	}
	if !h.Online(target.UUID) {
		return nil, errors.New("target node is offline")
	}
	if fetch.IsURLFetch() {
		// An archive URL is not a registry reference, so it never enters the
		// candidate chain: the node downloads and loads the archive instead.
		return h.loadImageArchiveURL(ctx, image, target, fetch)
	}
	candidates, err := h.pullCandidates(image, target, fetch.Copies)
	if err != nil || len(candidates) == 0 {
		if err == nil {
			err = errors.New("no image pull candidates")
		}
		return nil, err
	}
	if row, rowErr := h.store.ImageNode(image.ID, target.ID); rowErr == nil {
		// Force only gets past a finished pull. Two transfers for one image on
		// one node would race on the same tag.
		if imageRowActive(row) && !(fetch.Force && row.State == store.ImageStatePresent) {
			return row, nil
		}
	}
	// One id for the whole attempt chain: it is what a cancel names, and what
	// keeps a late status from an abandoned attempt out of the row that replaced
	// it.
	transferID := auth.NewUUID()
	session := h.session(target.UUID)
	walks := session != nil && session.HasCap(proto.CapImagePullCandidates)
	source, message := pullStartMessage(candidates, walks)
	row, err := h.store.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, ImageID: image.ID, NodeID: target.ID, State: store.ImageStatePulling, Source: source, TransferID: transferID, Message: message})
	if err != nil {
		return nil, err
	}
	if walks {
		all := make([]proto.ImagePullCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			all = append(all, candidate.ImagePullCandidate)
		}
		_, callErr := h.CallNode(ctx, target.UUID, proto.RPCImagePull, proto.ImagePullParams{EffectiveRef: candidates[0].Ref, CanonicalRef: image.ImageRef, TransferID: transferID, Candidates: all})
		if callErr == nil {
			h.watchDirectImageTransfer(image.ID, target.ID)
			return row, nil
		}
		message := boundedPullMessage("image pull failed to start: " + callErr.Error())
		_, _ = h.store.UpsertImageNode(&store.ImageNode{ID: row.ID, ImageVersionID: image.ID, ImageID: image.ID, NodeID: target.ID, State: store.ImageStateFailed, Source: candidates[0].Source, Message: message})
		return nil, callErr
	}
	immediateFailures := make([]string, 0, len(candidates))
	var productErr error
	for index, candidate := range candidates {
		if index > 0 {
			_, _ = h.store.UpsertImageNode(&store.ImageNode{ID: row.ID, ImageVersionID: image.ID, ImageID: image.ID, NodeID: target.ID, State: store.ImageStatePulling, Source: candidate.Source, TransferID: transferID, Message: boundedPullMessage(strings.Join(immediateFailures, " | ") + " | trying " + candidate.Source + " " + candidate.Ref)})
		}
		_, callErr := h.CallNode(ctx, target.UUID, proto.RPCImagePull, proto.ImagePullParams{EffectiveRef: candidate.Ref, CanonicalRef: image.ImageRef, TransferID: transferID})
		if callErr == nil {
			// The agent acknowledges the pull before the asynchronous Docker
			// operation finishes. Watch terminal failure and advance to the next
			// source without making the HTTP request wait for a large image.
			if len(candidates) > index+1 {
				go h.watchImagePullCandidates(image.ID, target.ID, target.UUID, row.ID, image.ImageRef, candidates, index)
			}
			// The node is doing the work, so the row only moves while the agent
			// reports. Watch for that reporting to stop, whatever the source.
			h.watchDirectImageTransfer(image.ID, target.ID)
			return row, nil
		}
		err = callErr
		if strings.Contains(strings.ToLower(err.Error()), "unknown rpc method: image.pull") {
			err = messages.NewProductError(
				messages.ErrorAgentUpgradeRequired,
				"The target node is running an older agent. Upgrade the agent before pulling images.",
				nil,
			)
			productErr = err
		}
		immediateFailures = append(immediateFailures, candidate.Source+": "+err.Error())
		if index+1 == len(candidates) {
			break
		}
	}
	message = "all image sources failed: " + strings.Join(immediateFailures, " | ")
	_, _ = h.store.UpsertImageNode(&store.ImageNode{ID: row.ID, ImageVersionID: image.ID, ImageID: image.ID, NodeID: target.ID, State: store.ImageStateFailed, Source: candidates[len(candidates)-1].Source, Message: message})
	if productErr != nil {
		return nil, productErr
	}
	return nil, errors.New(message)
}

// InspectImage reads an image's OCI metadata (entrypoint, cmd, env, declared
// volumes) from a node that holds it and records the node's immutable image
// id. The metadata is returned to the caller as a runtime fact and not stored:
// the catalog carries no launch configuration (ADR 0031).
// Sensitive environment values are redacted in successful responses.
func (h *Hub) InspectImage(ctx context.Context, imageID uint, targetNodeUUID string) (proto.ImageInspectResult, error) {
	image, err := h.store.GetImage(imageID)
	if err != nil {
		return proto.ImageInspectResult{}, err
	}
	if !h.Online(targetNodeUUID) {
		return proto.ImageInspectResult{}, errors.New("target node is offline")
	}
	raw, err := h.CallNode(ctx, targetNodeUUID, proto.RPCImageInspect, proto.ImageInspectParams{ImageRef: image.ImageRef})
	if err != nil {
		return proto.ImageInspectResult{}, err
	}
	var result proto.ImageInspectResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	status := proto.ImageNodeStatus{ImageRef: image.ImageRef, State: store.ImageStatePresent, ImageID: result.ImageID, Message: "image metadata inspected"}
	if node, nodeErr := h.store.NodeByUUID(targetNodeUUID); nodeErr == nil {
		// An operator may inspect an image that was loaded outside GPUWorks (or
		// before the image_nodes presence row was created). Create that row here
		// so the node's image id is not silently lost.
		if _, rowErr := h.store.ImageNode(image.ID, node.ID); errors.Is(rowErr, store.ErrNotFound) {
			if _, upsertErr := h.store.UpsertImageNode(&store.ImageNode{
				ImageVersionID: image.ID,
				ImageID:        image.ID,
				NodeID:         node.ID,
				State:          store.ImageStatePresent,
				Source:         "inspect",
				Message:        "image metadata inspected",
			}); upsertErr != nil {
				return result, upsertErr
			}
		} else if rowErr != nil {
			return result, rowErr
		}
		if applyErr := h.store.ApplyImageNodeStatus(image.ID, node.ID, status); applyErr != nil {
			return result, applyErr
		}
	}
	result.Env = commandlog.RedactEnv(result.Env)
	return result, nil
}

func (s *Session) hydrateRuntimeAssetContent(instances []proto.InstanceDesiredState) error {
	s.assetMu.RLock()
	defer s.assetMu.RUnlock()
	for instanceIndex := range instances {
		for assetIndex := range instances[instanceIndex].RuntimeAssets {
			asset := &instances[instanceIndex].RuntimeAssets[assetIndex]
			digest := strings.ToLower(strings.TrimSpace(asset.SHA256))
			if _, cached := s.assetDigests[digest]; cached || len(asset.Content) > 0 {
				continue
			}
			if s.hub == nil || s.hub.runtimeAssetLoader == nil || asset.PackageDigest == "" || asset.ModelID == "" {
				return fmt.Errorf("runtime asset %s has no content loader or persisted package reference", asset.File)
			}
			content, err := s.hub.runtimeAssetLoader(asset.PackageDigest, asset.ModelID, asset.File)
			if err != nil {
				return fmt.Errorf("load runtime asset %s for instance %s: %w", asset.File, instances[instanceIndex].UUID, err)
			}
			if asset.SizeBytes > 0 && int64(len(content)) != asset.SizeBytes {
				return fmt.Errorf("runtime asset %s size mismatch: got %d, want %d", asset.File, len(content), asset.SizeBytes)
			}
			actual := fmt.Sprintf("%x", sha256.Sum256(content))
			if actual != digest {
				return fmt.Errorf("runtime asset %s SHA-256 mismatch: got %s", asset.File, actual)
			}
			asset.Content = content
		}
	}
	return nil
}

func (s *Session) rememberAssetDigests(digests []string) {
	s.assetMu.Lock()
	defer s.assetMu.Unlock()
	for _, digest := range digests {
		digest = strings.ToLower(strings.TrimSpace(digest))
		if len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == "" {
			s.assetDigests[digest] = struct{}{}
		}
	}
}

func (s *Session) omitCachedAssetContent(instances []proto.InstanceDesiredState) {
	s.assetMu.RLock()
	defer s.assetMu.RUnlock()
	for instanceIndex := range instances {
		for assetIndex := range instances[instanceIndex].RuntimeAssets {
			digest := strings.ToLower(strings.TrimSpace(instances[instanceIndex].RuntimeAssets[assetIndex].SHA256))
			if _, cached := s.assetDigests[digest]; cached {
				instances[instanceIndex].RuntimeAssets[assetIndex].Content = nil
			}
		}
	}
}

// ---- server→agent RPC ----

// Call issues an RPC and waits for the response (or ctx/timeout).
func (s *Session) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, rpccontext.Timeout(ctx, 30*time.Second))
	defer cancel()
	return s.call(ctx, method, params)
}

// CallLong issues a cancellable RPC without the ordinary 30-second management
// timeout. It is used only for operations such as multi-terabyte model sync.
func (s *Session) CallLong(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return s.call(ctx, method, params)
}

func (s *Session) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.rpcSeq.Add(1)
	ch := make(chan proto.Envelope, 1)
	s.pendMu.Lock()
	s.pending[id] = ch
	s.pendMu.Unlock()
	defer func() {
		s.pendMu.Lock()
		delete(s.pending, id)
		s.pendMu.Unlock()
	}()

	req := rpcRequest{Method: method, Params: params}
	if err := s.send(proto.TRPCReq, id, req); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, errors.New("node session closed: " + s.NodeUUID)
	case env := <-ch:
		var resp rpcResponse
		if err := json.Unmarshal(env.D, &resp); err != nil {
			return nil, err
		}
		if resp.Error != "" {
			return nil, &rpcRemoteError{detail: resp.Error}
		}
		return resp.Result, nil
	}
}

// dispatchRPCResp routes an rpc.resp to the waiting caller.
func (s *Session) dispatchRPCResp(env proto.Envelope) {
	s.pendMu.Lock()
	ch := s.pending[env.ID]
	s.pendMu.Unlock()
	if ch != nil {
		select {
		case ch <- env:
		default:
		}
	}
}

type rpcRequest struct {
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// ---- hub-level routing ----

// session returns the live session for a node UUID, or nil.
func (h *Hub) session(uuid string) *Session {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sessions[uuid]
}

// Online reports whether a node currently has a live session.
func (h *Hub) Online(uuid string) bool { return h.session(uuid) != nil }

// Kick closes the live session for a node, if any. Token rotation and node
// deletion use this to make credential changes effective immediately rather
// than waiting for yamux keepalive or a reconnect.
func (h *Hub) Kick(uuid string) {
	if s := h.session(uuid); s != nil {
		s.Close()
	}
}

// PushToNode sends an incremental desired-state update to a node if connected.
func (h *Hub) PushToNode(uuid string, su proto.StateUpdate) error {
	s := h.session(uuid)
	if s == nil {
		return errors.New("node not connected: " + uuid)
	}
	return s.PushUpdate(su)
}

// SyncNode re-sends the full desired state to a node (used after big changes).
func (h *Hub) SyncNode(uuid string) error {
	s := h.session(uuid)
	if s == nil {
		return errors.New("node not connected: " + uuid)
	}
	return s.pushInitialState()
}

// OpenTunnel opens a data-plane stream to a node's agent, which bridges it to
// dst (e.g. 127.0.0.1:30000). Returns a net.Conn (the yamux stream).
//
// ctx bounds a detached data lane's wait for reattachment (see
// Session.dataLaneRoute); the proxy passes the request's context so a client
// disconnect or deadline aborts the wait instead of outliving it.
func (h *Hub) OpenTunnel(ctx context.Context, nodeUUID, dst string) (net.Conn, error) {
	s := h.session(nodeUUID)
	if s == nil {
		return nil, errors.New("node not connected: " + nodeUUID)
	}
	return s.openStream(ctx, proto.StreamHeader{Kind: proto.StreamTunnel, Dst: dst})
}

// OpenLogtail opens a log stream for an instance (ref = instance uuid). The
// returned conn yields the container's log bytes; close it to stop following.
func (h *Hub) OpenLogtail(ctx context.Context, nodeUUID, ref string) (net.Conn, error) {
	s := h.session(nodeUUID)
	if s == nil {
		return nil, errors.New("node not connected: " + nodeUUID)
	}
	return s.openStream(ctx, proto.StreamHeader{Kind: proto.StreamLogtail, Ref: ref})
}

// OpenLargeFileRead opens a bounded single-file read from a node's agent (path
// = absolute path on that node). The returned conn yields the file's bytes and
// ends at EOF; close it to abandon the transfer.
//
// It requires an already-attached data lane rather than waiting for one: the
// point of this stream is to move a file of up to proto.LargeFileReadMaxBytes
// without it sharing a connection with the node's heartbeats, status and
// desired state, and the caller treats "no lane" as "no fetch" and takes its
// other path immediately, so a multi-second wait here would only delay that
// fallback. Falling back to ctrl itself is refused unconditionally, in both
// the "none" and "detached" states.
func (h *Hub) OpenLargeFileRead(ctx context.Context, nodeUUID, path string) (net.Conn, error) {
	s := h.session(nodeUUID)
	if s == nil {
		return nil, errors.New("node not connected: " + nodeUUID)
	}
	if s.DataLaneState() != "attached" {
		return nil, errors.New("node " + nodeUUID + " has no data lane for a file transfer")
	}
	return s.openStream(ctx, proto.StreamHeader{Kind: proto.StreamLargeFileRead, Dst: path})
}

// OpenTerminal opens an interactive pty stream on a node's agent. The returned
// conn carries terminal frames (see proto.TermData and friends) in both
// directions; close it to end the session.
//
// Initial window size and the teardown grace period travel in the header so the
// agent needs no settings of its own, and so the shell starts at the right size
// rather than redrawing after the first resize frame.
func (h *Hub) OpenTerminal(nodeUUID, target, ref string, rows, cols uint16, graceSec int) (net.Conn, error) {
	s := h.session(nodeUUID)
	if s == nil {
		return nil, errors.New("node not connected: " + nodeUUID)
	}
	if target == "" {
		target = proto.TermTargetHost
	}
	// Terminal always rides ctrl (proto.LaneFor(StreamTerminal) == LaneCtrl), so
	// it never waits on the data lane; context.Background() here only feeds
	// yamux's OpenStream and never blocks.
	return s.openStream(context.Background(), proto.StreamHeader{
		Kind: proto.StreamTerminal, Target: target, Ref: ref,
		Rows: rows, Cols: cols, GraceSec: graceSec,
	})
}

// NodeCaps returns the capabilities a connected agent advertised, or nil when
// the node is not connected. The terminal entry point is greyed out for agents
// that predate webterm rather than opening a session that can only go blank.
func (h *Hub) NodeCaps(nodeUUID string) []string {
	s := h.session(nodeUUID)
	if s == nil {
		return nil
	}
	return s.Caps()
}

// CopyModelPath prefers direct transfer and can relay authorized TLS bytes on
// independent data lanes when both agents support it.
func (h *Hub) CopyModelPath(ctx context.Context, req proto.ModelSyncCopyRequest) (proto.ModelSyncCopyResult, error) {
	if h.session(req.SourceNodeUUID) == nil {
		return proto.ModelSyncCopyResult{}, errors.New("source node not connected: " + req.SourceNodeUUID)
	}
	if h.session(req.TargetNodeUUID) == nil {
		return proto.ModelSyncCopyResult{}, errors.New("target node not connected: " + req.TargetNodeUUID)
	}
	if req.TransferID == "" {
		req.TransferID = auth.NewUUID()
	}
	ctx, finish := h.beginObservedPeerTransfer(ctx, prepobs.PeerTransferSpec{TransferID: req.TransferID, Kind: "model", Operation: "copy", SourceNodeUUID: req.SourceNodeUUID, TargetNodeUUID: req.TargetNodeUUID, ResourceKey: req.Key})
	relay := h.observeArchiveRelay(req.TransferID, h.archiveRelayFor(req.SourceNodeUUID, req.TargetNodeUUID, "model"), func() {
		if file, err := h.store.ResolveModelFileKey(req.Key); err == nil {
			_, _ = h.store.MarkModelSyncWaiting(store.FileRef{ModelID: file.ModelID, StorageID: file.StorageID, NodeID: file.NodeID}, req.TransferID, "Peer relay is busy; waiting to retry the same source.", messages.ErrorPeerRelayBusy)
		}
	})
	result, err := coordinateModelCopy(ctx, req, auth.NewToken(32), h.callNodeForModelSync, relay)
	finish(err, result.InProgress)
	return result, err
}

func (h *Hub) ProbeModelPath(ctx context.Context, req proto.ModelSyncCopyRequest) error {
	if req.TransferID == "" {
		req.TransferID = auth.NewUUID()
	}
	ctx, finish := h.beginObservedPeerTransfer(ctx, prepobs.PeerTransferSpec{TransferID: req.TransferID, Kind: "model", Operation: "probe", SourceNodeUUID: req.SourceNodeUUID, TargetNodeUUID: req.TargetNodeUUID, ResourceKey: req.Key})
	result, err := coordinateModelCopyProbe(ctx, req, auth.NewToken(32), h.callNodeForModelSync, h.observeArchiveRelay(req.TransferID, h.archiveRelayFor(req.SourceNodeUUID, req.TargetNodeUUID, "model"), nil))
	finish(err, result.InProgress)
	return err
}

func (h *Hub) callNodeForModelSync(ctx context.Context, uuid, method string, params any, long bool) (json.RawMessage, error) {
	s := h.session(uuid)
	if s == nil {
		return nil, errors.New("node not connected: " + uuid)
	}
	if long {
		return s.CallLong(ctx, method, params)
	}
	return s.Call(ctx, method, params)
}

func (h *Hub) StopModelSync(ctx context.Context, nodeUUID, key, transferID string) error {
	h.CancelPeerTransfer(transferID)
	_, err := h.CallNode(ctx, nodeUUID, proto.RPCModelSyncStop, proto.ModelSyncStopParams{Key: key, TransferID: transferID})
	return err
}

// CallNode issues an RPC to a node's agent.
func (h *Hub) CallNode(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
	s := h.session(uuid)
	if s == nil {
		return nil, errors.New("node not connected: " + uuid)
	}
	result, err := s.Call(ctx, method, params)
	if method == proto.RPCNodeConnectionDiagnostics && h.session(uuid) != s {
		return nil, errors.New("connection changed during diagnostics")
	}
	return result, err
}

// pullStartMessage is what an image row says until the node reports. A node
// that walks the candidates itself may reorder a leading run of the image's
// own copies by how well it reaches them, so the first of them is not
// necessarily the one it tries; a rule endpoint, which leads unranked, is.
func pullStartMessage(candidates []imagePullCandidate, walks bool) (source, message string) {
	first := candidates[0]
	if walks && first.Rank && len(candidates) > 1 && candidates[1].Rank {
		return "", fmt.Sprintf("pulling: the node tries %d sources, nearest first", len(candidates))
	}
	return first.Source, "trying " + first.Source + " " + first.Ref
}

// OpenDiagnosticsRead opens a node's part of a diagnostics collection: a tar
// of the agent's log and the named debug sessions' files, which the agent
// chooses (proto.StreamDiagnosticsRead).
func (h *Hub) OpenDiagnosticsRead(ctx context.Context, nodeUUID string, sessions []string) (net.Conn, error) {
	s := h.session(nodeUUID)
	if s == nil {
		return nil, errors.New("node not connected: " + nodeUUID)
	}
	return s.openStream(ctx, proto.StreamHeader{Kind: proto.StreamDiagnosticsRead, Ref: strings.Join(sessions, ",")})
}
