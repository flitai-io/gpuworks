//go:build !linux || !dqlite

package app

import (
	"context"
	"errors"
	"time"
)

// ClusterMemberTimings requires the cluster runtime, which this build lacks.
func ClusterMemberTimings(context.Context, Config, time.Duration) ([]ClusterMemberTiming, error) {
	return nil, errors.New("reading the members' timing requires the native Linux dqlite server build")
}

// ClusterMembersListening requires the cluster runtime, which this build lacks.
func ClusterMembersListening(context.Context, Config, time.Duration) ([]string, int, error) {
	return nil, 0, errors.New("probing the members requires the native Linux dqlite server build")
}
