// Package agentupgrade owns durable, serial Agent upgrade requests.
package agentupgrade

import (
	"context"
	"time"
)

type Connection struct {
	SessionID string
	Version   string
	Caps      []string
	Ready     bool
}

type Observation struct {
	NodeUUID  string
	SessionID string
	Version   string
	State     string
	Error     string
	// Code classifies a failed State: proto.ErrorAgentUpgradeRolledBack,
	// proto.ErrorAgentUpgradeInProgress, or empty.
	Code string
	At   time.Time
}

type Transport interface {
	AgentUpgradeConnection(uuid string) (Connection, bool)
	PushAgentUpgrade(ctx context.Context, uuid, sessionID, name string, binary []byte) error
	RequestAgentUpgrade(ctx context.Context, uuid, sessionID string) error
}

// RejectedError is an explicit Agent refusal, distinct from a broken link
// whose installation outcome is unknown.
type RejectedError struct {
	Detail string
	Busy   bool
}

func (e *RejectedError) Error() string { return e.Detail }
