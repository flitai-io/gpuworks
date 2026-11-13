package deployment

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// inspectDeploymentModel uses the same source decision for preview and create.
// Preview only reads control-plane facts; create may read public metadata once.
// Neither mode proves an agent's data-plane reachability or starts a transfer.
func (s *Service) inspectDeploymentModel(ctx context.Context, model *store.Model, rules store.SourceRules, nodes []*store.Node, probeMetadata bool) (*dto.DiagnosticReport, error) {
	nodes = uniqueNodes(nodes)
	if model == nil || len(nodes) == 0 {
		return nil, nil
	}
	report := &dto.DiagnosticReport{}
	var firstErr error
	var needsMetadata []*store.Node
	for _, node := range nodes {
		src, err := s.d.Store.ModelSourceOn(model.ID, node, rules)
		if err != nil {
			check := modelDiagnostic(model, node, src, "facts", "unknown", "facts_unavailable", "Model location facts could not be read")
			report.Checks = append(report.Checks, check)
			blocked := modelDiagnostic(model, node, src, "source", "blocked", "facts_unavailable", "Model source selection was not completed")
			blocked.BlockedBy = []string{check.ID}
			report.Checks = append(report.Checks, blocked)
			if firstErr == nil {
				firstErr = wrapProductError(messages.ErrorModelSourceUnavailable, "model location facts could not be read", check.Message.Params, err)
			}
			continue
		}
		appendModelLocationChecks(report, model, node, src)
		status, reason, message := "unknown", "metadata_not_checked", "Public metadata has not been checked by the management node"
		switch src.Action {
		case store.SourceReady:
			status, reason, message = "passed", "source_ready", "A complete model copy is recorded as readable on the node"
		case store.SourceFillCache:
			status, reason, message = "pending", "cache_fill_required", "The shared model must be copied into the node cache at launch"
		case store.SourceWaitShared:
			status, reason, message = "pending", "shared_transfer_pending", "A transfer into the shared storage is in progress"
		default:
			if acceptedModelCopy(src) {
				status, reason, message = "pending", "copy_in_progress", "An accepted copy is in progress; the model is not ready yet"
				break
			}
			if src.Action == store.SourceAcquire {
				peer, peerErr := s.modelCopyPeer(model.ID, node)
				if errors.Is(peerErr, netplan.ErrNoStoragePath) {
					policy := modelDiagnostic(model, node, src, "peer", "not_applicable", "no_storage_path", "The peer path is not permitted by the storage-sync network policy")
					policy.Message.Code = messages.ErrorNoStoragePath
					var productErr *messages.ProductError
					if errors.As(peerErr, &productErr) {
						for key, value := range productErr.Params() {
							policy.Message.Params[key] = value
						}
					}
					if !modelHasPublicSource(model) {
						policy.Status = "failed"
						report.Checks = append(report.Checks, policy)
						blocked := modelDiagnostic(model, node, src, "source", "blocked", "peer_transport_required", "Model acquisition is blocked by the storage-sync network policy")
						blocked.BlockedBy = []string{policy.ID}
						blocked.Message.Params["source"], blocked.Message.Params["target"] = policy.Message.Params["source"], policy.Message.Params["target"]
						report.Checks = append(report.Checks, blocked)
						if firstErr == nil {
							firstErr = wrapProductError(messages.ErrorModelSourceUnavailable, policy.Message.Message, policy.Message.Params, peerErr)
						}
						continue
					}
					report.Checks = append(report.Checks, policy)
				} else if peerErr != nil {
					status, reason, message = "unknown", "facts_unavailable", "Model peer facts could not be read"
					if firstErr == nil {
						firstErr = wrapProductError(messages.ErrorModelSourceUnavailable, message, modelDiagnosticParams(model, node, src, reason), peerErr)
					}
					break
				}
				if peer != "" {
					status, reason, message = "unknown", "peer_available", "A qualified peer has a complete copy; its data-plane connection has not been tested"
					check := modelDiagnostic(model, node, src, "peer", status, reason, message)
					check.Message.Params["peer"] = peer
					report.Checks = append(report.Checks, check)
					continue
				}
			}
			if !modelHasPublicSource(model) || (src.Action == store.SourceRefused && src.Reason != store.LocationModelMissingInStore) {
				reason = src.Reason
				if reason == "" {
					reason = src.Location.Reason
				}
				if reason == "" {
					reason = "no_acquisition_path"
				}
				status, message = "failed", "No permitted model acquisition path is available for this node"
				if firstErr == nil {
					firstErr = productError(messages.ErrorModelSourceUnavailable, message, modelDiagnosticParams(model, node, src, reason))
				}
				break
			}
			needsMetadata = append(needsMetadata, node)
		}
		report.Checks = append(report.Checks, modelDiagnostic(model, node, src, "source", status, reason, message))
	}
	if len(needsMetadata) > 0 {
		check := modelDiagnostic(model, nil, store.ModelSource{}, "metadata", "unknown", "metadata_not_checked", "Public metadata has not been checked by the management node")
		check.Message.Params["observer"] = "management_node"
		if probeMetadata && s.d.FetchModelMeta != nil {
			readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := s.FetchModelMeta(readCtx, model.Source, model.SourceID, model.Revision, model.ArtifactSubfolder, model.SourceURL)
			cancel()
			if err != nil {
				check.Status = "failed"
				check.Message.Params["reason"] = metadataFailureReason(err)
				check.Message.Message = "The management node could not read the model metadata"
				for i := range report.Checks {
					dependent := &report.Checks[i]
					if dependent.Message.Params["reason"] == "metadata_not_checked" {
						dependent.Status = "blocked"
						dependent.BlockedBy = []string{check.ID}
					}
				}
				if firstErr == nil {
					firstErr = wrapProductError(messages.ErrorModelSourceUnavailable, "model source is unavailable; deployment was not created", check.Message.Params, err)
				}
			} else {
				// The fetcher deliberately tolerates missing metadata. A nil error
				// does not prove files are downloadable, even on this observer.
				check.Message.Params["reason"] = "metadata_only"
				check.Message.Message = "Metadata admission completed; model-file and agent connectivity are not verified"
			}
		}
		report.Checks = append(report.Checks, check)
	}
	if firstErr != nil {
		return report, WithDiagnostics(firstErr, report)
	}
	return report, nil
}

func modelHasPublicSource(model *store.Model) bool {
	return model != nil && !strings.EqualFold(strings.TrimSpace(model.Source), "local") && strings.TrimSpace(model.SourceID) != ""
}

func acceptedModelCopy(src store.ModelSource) bool {
	file := src.Location.File
	return src.Action == store.SourceAcquire && store.TransferUnderWay(file) && file.Acquisition == proto.ModelAcquireCopy && file.TransferID != ""
}

func metadataFailureReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "source_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "source_timeout"
	case errors.Is(err, modelmeta.ErrTokenRejected):
		return "source_authentication"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "source_timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "source_dns_failed"
	}
	var verification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	if errors.As(err, &verification) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalidCertificate) {
		return "source_tls_failed"
	}
	var connection *net.OpError
	if errors.As(err, &connection) {
		return "source_connect_failed"
	}
	return "source_unavailable"
}

func modelDiagnosticParams(model *store.Model, node *store.Node, src store.ModelSource, reason string) map[string]any {
	params := map[string]any{"model_id": model.ID, "model": model.DisplayName(), "reason": reason, "observer": "control_plane"}
	if node != nil {
		params["node_id"], params["node_uuid"], params["node"] = node.ID, node.UUID, nodeDisplayName(node)
	}
	if storage := src.Location.Storage; storage.ID != 0 {
		params["storage_id"], params["storage"] = storage.ID, storage.Name
	}
	if src.Action != "" {
		params["action"] = src.Action
	}
	return params
}

func modelDiagnostic(model *store.Model, node *store.Node, src store.ModelSource, phase, status, reason, message string) dto.DiagnosticCheck {
	nodeID := uint(0)
	if node != nil {
		nodeID = node.ID
	}
	return dto.DiagnosticCheck{
		ID: fmt.Sprintf("model-%s:%d:%d:%d", phase, model.ID, nodeID, src.Location.Storage.ID), Status: status,
		Message: dto.ProductMessage{Code: messages.ErrorModelSourceUnavailable, Params: modelDiagnosticParams(model, node, src, reason), Message: message},
	}
}

func appendModelLocationChecks(report *dto.DiagnosticReport, model *store.Model, node *store.Node, src store.ModelSource) {
	loc := src.Location
	if loc.Storage.ID == 0 {
		return
	}
	file := loc.File
	cachedReady := loc.Storage.Type == proto.StorageNFSCache && src.Action == store.SourceReady && loc.File.Ready()
	if loc.Storage.Type != proto.StorageLocal && !cachedReady {
		file = loc.Shared
	}
	status, reason, message := "unknown", store.LocationNotOnNode, "No complete model copy is recorded on this node"
	if loc.Storage.Type != proto.StorageLocal {
		reason, message = store.LocationModelMissingInStore, "No complete model copy is recorded in this shared storage"
	}
	if file.Ready() {
		status, reason, message = "passed", "model_record_ready", "A complete model copy is recorded in this storage"
	} else if store.TransferUnderWay(file) {
		status, reason, message = "pending", "model_transfer_pending", "A model transfer is in progress"
	}
	check := modelDiagnostic(model, node, src, "record", status, reason, message)
	if file != nil {
		check.Message.Params["state"] = file.State
		if file.TransferID != "" {
			check.Message.Params["transfer_id"] = file.TransferID
		}
	}
	report.Checks = append(report.Checks, check)
	if loc.Storage.Type == proto.StorageLocal || cachedReady {
		return
	}
	status, reason, message = "unknown", "storage_state_unknown", "No node storage-access report is available"
	if state := loc.NodeState; state != nil {
		switch state.State {
		case proto.StorageStateReady:
			status, reason, message = "passed", "storage_ready", "The node reports that this storage is ready"
		case proto.StorageStateMounting:
			status, reason, message = "pending", "storage_mounting", "The node is mounting this storage; model-path access must wait"
		case proto.StorageStateFailed, proto.StorageStateUnresponsive, proto.StorageStateUnsupported:
			status, reason, message = "failed", state.Reason, "The node reports that this storage is not usable"
			if reason == "" {
				switch state.State {
				case proto.StorageStateUnresponsive:
					reason = "storage_unresponsive"
				case proto.StorageStateUnsupported:
					reason = "storage_unsupported"
				default:
					reason = store.LocationStorageUnavailable
				}
			}
		default:
			message = "The node storage-access state is unknown"
		}
	}
	access := modelDiagnostic(model, node, src, "storage", status, reason, message)
	access.Message.Code = messages.ErrorStorageUnavailableOnNode
	if loc.NodeState != nil {
		access.Message.Params["observer"] = "agent_report"
		access.Message.Params["state"] = loc.NodeState.State
	}
	report.Checks = append(report.Checks, access)
	if status != "passed" {
		blocked := modelDiagnostic(model, node, src, "path", "blocked", "storage_access_required", "Model-path readability has not been checked because storage access is unavailable")
		blocked.BlockedBy = []string{access.ID}
		report.Checks = append(report.Checks, blocked)
	}
}
