package store

import (
	"encoding/json"
	"strings"
	"time"
)

// Serving forward modes the gateway reports per node.
const (
	ServingForwardDirect      = "direct"
	ServingForwardUnreachable = "unreachable"
)

// ServingForwardStatus is the gateway's last outcome reaching a node over its
// serving network: direct, or back on the agent tunnel with the reason.
type ServingForwardStatus struct {
	Mode   string    `json:"mode"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// NodeServingForward decodes Node.ServingForward; ok is false when the gateway
// has not tried the node yet.
func NodeServingForward(raw string) (ServingForwardStatus, bool) {
	var st ServingForwardStatus
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &st) != nil || st.Mode == "" {
		return ServingForwardStatus{}, false
	}
	return st, true
}

// SetNodeServingForward records the gateway's outcome when it differs from
// the stored one, so a request stream does not turn into a write stream. It
// publishes nothing: the status is informational and a node event would make
// the controller re-plan every deployment.
func (s *Store) SetNodeServingForward(uuid string, st ServingForwardStatus) error {
	var node Node
	if err := s.db.Select("id", "serving_forward").Where("uuid = ?", uuid).First(&node).Error; err != nil {
		return err
	}
	if old, ok := NodeServingForward(node.ServingForward); ok && old.Mode == st.Mode && old.Reason == st.Reason {
		return nil
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := s.db.Model(&Node{}).Where("id = ?", node.ID).Update("serving_forward", string(b)).Error; err != nil {
		return err
	}
	// No event, but the node list shows it: the next FleetNodes reads it.
	s.noteNodeChange("serving_forward")
	return nil
}
