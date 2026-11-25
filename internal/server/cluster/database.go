// Package cluster owns the replicated database process and its connections.
package cluster

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

var ErrUnavailable = errors.New("replicated database requires a Linux build with the dqlite build tag")

// DatabaseConfig contains transport settings, not admission credentials. The
// caller must authorize a joining member before giving it cluster TLS material.
type DatabaseConfig struct {
	Dir       string
	Address   string
	Seeds     []string
	Name      string
	ListenTLS *tls.Config
	DialTLS   *tls.Config
	// TLSCertPEM, TLSKeyPEM and TLSCACertPEM carry the member certificate, its
	// key and the pinned authority behind ListenTLS and DialTLS. The database
	// process runs outside this one and cannot read an in-memory tls.Config.
	TLSCertPEM   []byte
	TLSKeyPEM    []byte
	TLSCACertPEM []byte
	// TransportPolicyPath names the durable authorization the database
	// process checks on every accepted handshake. It is required with TLS:
	// the in-memory ListenTLS callbacks do not cross the process boundary.
	TransportPolicyPath string
	// StorageTier is the cluster's timing profile. Empty takes the tier this
	// process was told its installation records (UseStorageTier); every
	// member of one cluster must start its node with the same tier. Naming
	// one here starts the node with it and does not change what the
	// process was told, so nothing outside the node follows it: commands
	// tell the process and leave this empty.
	StorageTier storagetier.Tier
}

type Member struct {
	ID      uint64
	Address string
	Role    string
}

// Database separates normal requests from decisions that must stay local.
// Scheduling may be opened on a follower, but cannot execute through a remote
// leader. Close releases both pools before stopping the embedded database.
type Database struct {
	transportPolicy         *identity.RuntimeTransportPolicy
	SQL                     *sql.DB
	Scheduling              *sql.DB
	ID                      uint64
	Address                 string
	leader                  func(context.Context) (Member, error)
	members                 func(context.Context) ([]Member, error)
	quorum                  func(context.Context) (QuorumProof, error)
	quorumSnapshot          func(context.Context, func(context.Context, *sql.Tx, QuorumProof) error) (QuorumProof, error)
	closeQuorum             func()
	transfer                func(context.Context, uint64) error
	remove                  func(context.Context, uint64) error
	assign                  func(context.Context, uint64, string) error
	ready                   func(context.Context) error
	closed                  atomic.Bool
	stop                    func() error
	closeOnce               sync.Once
	closeErr                error
	transportClusterID      string
	transportCAFingerprint  string
	transportKeyFingerprint string
}

// TransportMemberKey is the public-key identity actually loaded by the authorized transport.
func (d *Database) TransportMemberKey() string { return d.transportKeyFingerprint }

func (d *Database) Leader(ctx context.Context) (Member, error)    { return d.leader(ctx) }
func (d *Database) Members(ctx context.Context) ([]Member, error) { return d.members(ctx) }
func (d *Database) Transfer(ctx context.Context, id uint64) error { return d.transfer(ctx, id) }

// Remove asks the current leader to drop a member. It does not decide whether
// the cluster can still form a majority; callers check that first.
func (d *Database) Remove(ctx context.Context, id uint64) error {
	if d == nil || d.remove == nil {
		return ErrUnavailable
	}
	return d.remove(ctx, id)
}

// Assign asks the current leader to change a member's role. Callers decide
// whether the resulting voter set still has a majority.
func (d *Database) Assign(ctx context.Context, id uint64, role string) error {
	if d == nil || d.assign == nil {
		return ErrUnavailable
	}
	return d.assign(ctx, id, role)
}

func (d *Database) Close() error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		d.closed.Store(true)
		if d.closeQuorum != nil {
			d.closeQuorum()
		}
		d.closeErr = errors.Join(d.Scheduling.Close(), d.SQL.Close(), d.stop())
	})
	return d.closeErr
}

func (c DatabaseConfig) validate() error {
	if c.Dir == "" || c.Name == "" || filepath.Base(c.Name) != c.Name || c.Name == "." || c.Name == ".." {
		return errors.New("database directory and a simple database name are required")
	}
	if _, err := storagetier.Parse(string(c.StorageTier)); err != nil {
		return err
	}
	secured := c.ListenTLS != nil && c.DialTLS != nil
	if (c.ListenTLS == nil) != (c.DialTLS == nil) {
		return errors.New("both listening and dialing TLS configurations are required")
	}
	if secured {
		if c.ListenTLS.ClientAuth != tls.RequireAndVerifyClientCert || c.ListenTLS.ClientCAs == nil || c.DialTLS.RootCAs == nil || c.DialTLS.InsecureSkipVerify {
			return errors.New("cluster TLS must verify both member and server certificates")
		}
		if c.ListenTLS.MinVersion < tls.VersionTLS12 || c.DialTLS.MinVersion < tls.VersionTLS12 {
			return errors.New("cluster TLS requires TLS 1.2 or later")
		}
	}
	for _, address := range append([]string{c.Address}, c.Seeds...) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("invalid database address %q: %w", address, err)
		}
		p, err := strconv.Atoi(port)
		if host == "" || err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("database address must have an explicit host and port: %q", address)
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsMulticast()) {
			return fmt.Errorf("database address must identify a reachable member: %q", address)
		}
		if !secured && !net.ParseIP(host).IsLoopback() {
			return errors.New("database transport outside loopback requires mutual TLS")
		}
	}
	return nil
}

// ApplyTransportPolicySnapshot allows only additive authorization changes on an
// explicitly authorized transport. It does not grant Raft membership/readiness.
func (d *Database) ApplyTransportPolicySnapshot(snapshot identity.TransportPolicy) (identity.TransportPolicy, error) {
	if d == nil || d.transportPolicy == nil {
		return identity.TransportPolicy{}, errors.New("database does not own a runtime transport policy")
	}
	return d.transportPolicy.Apply(snapshot)
}

// TransportPolicySnapshot returns the policy actually activated by this owner,
// never a fresh file read that could mistake uncertain publication for activation.
func (d *Database) TransportPolicySnapshot() (identity.TransportPolicy, error) {
	if d == nil || d.transportPolicy == nil {
		return identity.TransportPolicy{}, errors.New("database does not own a runtime transport policy")
	}
	return d.transportPolicy.Snapshot()
}

// OpenDatabase starts the database and waits for ordinary SQL access. A failed
// wait closes the instance; use StartDatabase when startup must survive no quorum.
func OpenDatabase(ctx context.Context, cfg DatabaseConfig) (*Database, error) {
	db, err := StartDatabase(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := db.WaitReady(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// WaitReady waits for upstream startup work and ordinary SQL access. It does not
// prove local voting rights or catch-up. Failure leaves the instance running;
// callers may wait again. Only Close terminates an instance returned by Start.
func (d *Database) WaitReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == nil || d.ready == nil {
		return ErrUnavailable
	}
	if d.closed.Load() {
		return errors.New("database is closed")
	}
	err := d.ready(ctx)
	if d.closed.Load() {
		return errors.Join(err, errors.New("database is closed"))
	}
	return err
}
