//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// ClusterMemberTimings asks every other member of this installation's
// cluster which storage tier it runs. The members and their enrollment
// listeners come from the leader's membership, as the status report learns
// them; each is then asked on its own listener with this member's key, for
// the tier it runs and the tier it records. A member that cannot be asked,
// or whose program does not say, carries the reason instead of a tier: a
// caller that needs every member's timing must not guess one.
func ClusterMemberTimings(ctx context.Context, cfg Config, timeout time.Duration) ([]ClusterMemberTiming, error) {
	if timeout <= 0 {
		return nil, errors.New("reading the members' timing requires a positive timeout")
	}
	report, err := readClusterStatus(cfg)
	if err != nil {
		return nil, err
	}
	if report.pin == "" || len(report.cert) == 0 || len(report.key) == 0 {
		return nil, errors.New("this installation holds no cluster member identity")
	}
	facts, ok := statusQuorumFetch(ctx, report, timeout)
	if !ok {
		return nil, errors.New("the cluster's membership could not be read from any member's enrollment listener")
	}
	var timings []ClusterMemberTiming
	for _, member := range facts.Members {
		if member.ID == report.id {
			continue
		}
		timing := ClusterMemberTiming{ID: member.ID, Address: member.Address}
		timing.Tier, timing.Recorded, timing.Unread = memberTiming(ctx, report, member.EnrollmentOrigin, timeout)
		timings = append(timings, timing)
	}
	return timings, nil
}

func memberTiming(ctx context.Context, report clusterStatus, origin string, timeout time.Duration) (running, recorded, unread string) {
	if !enrollment.CanonicalEnrollmentOrigin(origin) {
		return "", "", "the membership names no enrollment listener for it"
	}
	client, err := enrollment.NewClient(origin+enrollmentCertificatePath, report.clusterID, report.pin, report.protocol)
	if err != nil {
		return "", "", err.Error()
	}
	defer client.Close()
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := client.ClusterUpgrade(callCtx, report.cert, report.key, enrollment.ClusterUpgradeStatus)
	switch {
	case err != nil:
		return "", "", "it did not answer: " + err.Error()
	case response.StorageTier == "":
		return "", "", "its program does not say which storage tier it runs"
	case response.RecordedStorageTier == "":
		return "", "", "it does not say which storage tier it records"
	}
	return response.StorageTier, response.RecordedStorageTier, ""
}

// ClusterMembersListening names the other members of this installation's
// cluster whose database port accepts a connection now: the members that
// are running, whether or not the cluster has a leader. The members are the
// ones this installation's own record of the cluster lists, so the answer
// needs nobody else's; a command that must find the whole cluster stopped
// asks this and not the leader. The second result is how many other members
// the record lists.
func ClusterMembersListening(ctx context.Context, cfg Config, timeout time.Duration) (listening []string, known int, err error) {
	if timeout <= 0 {
		return nil, 0, errors.New("probing the members requires a positive timeout")
	}
	report, err := readClusterStatus(cfg)
	if err != nil {
		return nil, 0, err
	}
	for _, peer := range probePeerDatabases(ctx, report, timeout) {
		known++
		if peer.database == "open" {
			listening = append(listening, peer.address)
		}
	}
	return listening, known, nil
}
