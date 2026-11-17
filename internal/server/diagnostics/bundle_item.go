package diagnostics

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Bundle item ids: which part of a collection an item is. They follow from
// the item's path, so the manifest does not store them.
const (
	BundleItemServerDisk    = "server_disk"
	BundleItemServerLog     = "server_log"
	BundleItemDebugFile     = "debug_file"
	BundleItemNode          = "node"
	BundleItemNodeAgentLog  = "node_agent_log"
	BundleItemNodeDoctor    = "node_doctor"
	BundleItemNodeDebugFile = "node_debug_file"
	BundleItemInstanceLog   = "instance_log"
	BundleItemRecords       = "records"
	BundleItemRecord        = "record"
)

// ID is the lower_snake id of the part Path names.
func (it BundleItem) ID() string {
	parts := strings.Split(it.Path, "/")
	switch {
	case it.Path == "server/disk.json":
		return BundleItemServerDisk
	case it.Path == "server/gpuworks-server.log":
		return BundleItemServerLog
	case parts[0] == "debug":
		return BundleItemDebugFile
	case parts[0] == "nodes" && len(parts) == 2:
		return BundleItemNode
	case parts[0] == "nodes" && len(parts) == 3 && parts[2] == "agent.log":
		return BundleItemNodeAgentLog
	case parts[0] == "nodes" && len(parts) == 3 && parts[2] == "doctor.json":
		return BundleItemNodeDoctor
	case parts[0] == "nodes" && len(parts) > 3 && parts[2] == "debug":
		return BundleItemNodeDebugFile
	case parts[0] == "instances":
		return BundleItemInstanceLog
	case it.Path == "db":
		return BundleItemRecords
	case parts[0] == "db":
		return BundleItemRecord
	}
	return strings.ReplaceAll(parts[0], "-", "_")
}

// Node is the node a node part belongs to; empty for other parts.
func (it BundleItem) Node() string {
	if parts := strings.Split(it.Path, "/"); parts[0] == "nodes" && len(parts) > 1 {
		return parts[1]
	}
	return ""
}

// Message is the English fallback for Code.
func (it BundleItem) Message() string {
	switch it.Code {
	case messages.CheckDiagnosticsItemTruncated:
		return it.Path + " reached its size limit; only part was kept"
	case messages.CheckDiagnosticsNodeOffline:
		return "the node is offline; " + it.Path + " was not collected"
	case messages.CheckDiagnosticsAgentUnsupported:
		return "the node's agent does not support this read; " + it.Path + " was not collected"
	case messages.CheckDiagnosticsItemReadFailed:
		return it.Path + " could not be read"
	case messages.CheckDiagnosticsItemStageFailed:
		return it.Path + " could not be staged on the server's disk"
	}
	return ""
}

func collectedItem(path string, n int64, truncated bool) BundleItem {
	if truncated {
		return BundleItem{Path: path, Status: proto.CheckWarn, Code: messages.CheckDiagnosticsItemTruncated, Bytes: n}
	}
	return BundleItem{Path: path, Status: proto.CheckPass, Bytes: n}
}

func failedItem(path, code string, err error, n int64) BundleItem {
	return BundleItem{Path: path, Status: proto.CheckFail, Code: code, Evidence: err.Error(), Bytes: n}
}

func skippedItem(path, code string) BundleItem {
	return BundleItem{Path: path, Status: proto.CheckSkip, Code: code}
}

// UnmarshalJSON reads both the current item and the one manifests were
// written with before items were checks: {name, state collected|skipped|
// failed, reason, truncated}, where a failed item's reason was the error text
// and a skipped one's was offline or unsupported.
func (it *BundleItem) UnmarshalJSON(raw []byte) error {
	type current BundleItem
	var wire struct {
		current
		Name      string `json:"name"`
		State     string `json:"state"`
		Reason    string `json:"reason"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	*it = BundleItem(wire.current)
	if it.Status != "" || wire.State == "" {
		return nil
	}
	it.Path = wire.Name
	switch wire.State {
	case "collected":
		*it = collectedItem(wire.Name, wire.Bytes, wire.Truncated)
	case "skipped":
		it.Status, it.Code = proto.CheckSkip, messages.CheckDiagnosticsAgentUnsupported
		if wire.Reason == "offline" {
			it.Code = messages.CheckDiagnosticsNodeOffline
		}
	default:
		it.Status, it.Code, it.Evidence = proto.CheckFail, messages.CheckDiagnosticsItemReadFailed, wire.Reason
	}
	return nil
}
