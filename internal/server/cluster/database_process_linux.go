//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/canonical/go-dqlite/v3/client"
	"github.com/canonical/go-dqlite/v3/driver"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// StartDatabase runs the dqlite node in the gpuworks-dqlite child process and
// opens SQL pools against it without waiting for quorum. This process holds no
// native database code. After success the caller owns Close; ctx cancellation
// does not stop the node.
func StartDatabase(ctx context.Context, cfg DatabaseConfig) (_ *Database, err error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The child records its identity in this directory and does not create it.
	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		return nil, err
	}
	args := []string{"--dir", cfg.Dir, "--address", cfg.Address}
	if cfg.ListenTLS != nil {
		if cfg.TransportPolicyPath == "" {
			return nil, errors.New("database transport over TLS requires a transport policy file")
		}
		tlsDir, err := writeHelperTLS(cfg)
		if err != nil {
			return nil, err
		}
		socket, err := databaseUnixSocket(cfg.Dir)
		if err != nil {
			return nil, fmt.Errorf("database transport directory: %w", err)
		}
		// Independent clusters share a bootstrap ID; the native endpoint must be
		// scoped to storage.
		args = append(args,
			"--cert", filepath.Join(tlsDir, "cert.pem"),
			"--key", filepath.Join(tlsDir, "key.pem"),
			"--ca", filepath.Join(tlsDir, "ca.pem"),
			"--policy", cfg.TransportPolicyPath,
			"--unix", socket)
	}
	if len(cfg.Seeds) != 0 {
		args = append(args, "--seeds", strings.Join(cfg.Seeds, ","))
	}
	tier := cfg.storageTier()
	args = append(args, storageTierArguments(tier)...)
	process, info, err := startHelperUntilReady(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("start dqlite: %w", err)
	}
	defer func() {
		if err != nil {
			_ = process.stop(0)
		}
	}()
	if info.Address != cfg.Address {
		return nil, fmt.Errorf("stored database address %q differs from configured address %q", info.Address, cfg.Address)
	}
	shortest, longest := tier.ElectionTimer()
	slog.Info("database node started", "storage_tier", string(tier), "network_latency_ms", tier.NetworkLatency().Milliseconds(), "election_timer_min_ms", shortest.Milliseconds(), "election_timer_max_ms", longest.Milliseconds())
	self := client.NodeInfo{ID: info.ID, Address: info.Address, Role: client.Voter}
	// The child accepts its private socket only from itself, so every
	// connection from this process uses the member address.
	dial := func(ctx context.Context, address string) (net.Conn, error) {
		return dialMember(ctx, cfg.DialTLS, address)
	}
	localDial := func(ctx context.Context, address string) (net.Conn, error) {
		// The driver also dials addresses learned during leader discovery, so
		// restricting only the initial seed list would not fence scheduling.
		if address != info.Address {
			return nil, fmt.Errorf("scheduling database rejects remote address %q", address)
		}
		return dial(ctx, address)
	}
	// Ordinary requests follow the leader on any recorded member.
	members := directoryMembers{dir: cfg.Dir, self: self}
	normalDriver, err := driver.New(members, driver.WithDialFunc(dial))
	if err != nil {
		return nil, err
	}
	normalConnector, err := normalDriver.OpenConnector(cfg.Name)
	if err != nil {
		return nil, err
	}
	localSeeds := client.NewInmemNodeStore()
	if err := localSeeds.Set(ctx, []client.NodeInfo{self}); err != nil {
		return nil, err
	}
	localDriver, err := driver.New(localSeeds, driver.WithDialFunc(func(ctx context.Context, address string) (net.Conn, error) {
		return dialQuorumSQLSocket(ctx, address, localDial)
	}))
	if err != nil {
		return nil, err
	}
	localConnector, err := localDriver.OpenConnector(cfg.Name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	normal := openTransactionSQL(normalConnector, true)
	normal.SetMaxOpenConns(4)
	// The local-only scheduling pool deliberately has no writer admission.
	local := openTransactionSQL(localConnector, false)
	local.SetMaxOpenConns(1)
	db := &Database{SQL: normal, Scheduling: local, ID: info.ID, Address: info.Address, stop: func() error {
		return process.stop(helperStopWait)
	}}
	openLeader := func(ctx context.Context) (*client.Client, error) {
		return client.FindLeader(ctx, members, client.WithDialFunc(dial))
	}
	db.leader = func(ctx context.Context) (Member, error) {
		c, err := openLeader(ctx)
		if err != nil {
			return Member{}, err
		}
		defer c.Close()
		leader, err := c.Leader(ctx)
		if err != nil {
			return Member{}, err
		}
		if leader == nil {
			return Member{}, errors.New("database has no leader")
		}
		return Member{ID: leader.ID, Address: leader.Address, Role: leader.Role.String()}, nil
	}
	db.members = func(ctx context.Context) ([]Member, error) {
		c, err := openLeader(ctx)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		listed, err := c.Cluster(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Member, 0, len(listed))
		for _, m := range listed {
			out = append(out, Member{ID: m.ID, Address: m.Address, Role: m.Role.String()})
		}
		return out, nil
	}
	db.ready = func(ctx context.Context) error {
		if err := waitDatabase(ctx, normal); err != nil {
			return err
		}
		// Ordinary SQL already works through the other members while the child
		// is still asking the leader to admit it. The child keeps its join
		// marker until that request has succeeded.
		return waitJoined(ctx, cfg.Dir)
	}
	db.transfer = func(ctx context.Context, id uint64) error {
		c, err := openLeader(ctx)
		if err != nil {
			return err
		}
		defer c.Close()
		return c.Transfer(ctx, id)
	}
	db.remove = func(ctx context.Context, id uint64) error {
		c, err := openLeader(ctx)
		if err != nil {
			return err
		}
		defer c.Close()
		return c.Remove(ctx, id)
	}
	db.assign = func(ctx context.Context, id uint64, role string) error {
		var nodeRole client.NodeRole
		switch role {
		case "voter":
			nodeRole = client.Voter
		case "spare":
			nodeRole = client.Spare
		case "stand-by":
			nodeRole = client.StandBy
		default:
			return fmt.Errorf("unknown member role %q", role)
		}
		c, err := openLeader(ctx)
		if err != nil {
			return err
		}
		defer c.Close()
		return c.Assign(ctx, id, nodeRole)
	}
	bindDatabaseQuorumReader(db, dial, func(ctx context.Context) (*sql.DB, func() error, error) {
		connector, err := localDriver.OpenConnector(cfg.Name)
		if err != nil {
			return nil, nil, err
		}
		return openQuorumSQL(ctx, connector)
	})
	// From here an exit of the child takes this process down with it.
	process.guard()
	return db, nil
}

func waitDatabase(ctx context.Context, db *sql.DB) error {
	for {
		err := db.PingContext(ctx)
		if err == nil {
			return nil
		}
		if waitErr := pause(ctx); waitErr != nil {
			return fmt.Errorf("database readiness: %w (last connection error: %v)", waitErr, err)
		}
	}
}

func waitJoined(ctx context.Context, dir string) error {
	marker := filepath.Join(dir, "join")
	for {
		_, err := os.Lstat(marker)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if waitErr := pause(ctx); waitErr != nil {
			return fmt.Errorf("database membership: %w (join marker: %v)", waitErr, err)
		}
	}
}

// writeHelperTLS stores the member certificate beside the raft directory.
// dqlite treats the raft directory itself as its own.
func writeHelperTLS(cfg DatabaseConfig) (string, error) {
	if len(cfg.TLSCertPEM) == 0 || len(cfg.TLSKeyPEM) == 0 || len(cfg.TLSCACertPEM) == 0 {
		return "", errors.New("database process requires the member certificate, key and authority in PEM")
	}
	dir := cfg.Dir + ".helper-tls"
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	for _, file := range []struct {
		name string
		body []byte
	}{{"cert.pem", cfg.TLSCertPEM}, {"key.pem", cfg.TLSKeyPEM}, {"ca.pem", cfg.TLSCACertPEM}} {
		path := filepath.Join(dir, file.name)
		// A rewrite must not widen an existing file or follow a planted link.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		_, writeErr := f.Write(file.body)
		if err := errors.Join(writeErr, f.Close()); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// storageTierArguments names the tier to the database process by the latency
// it elects on. The standard tier's own latency adds nothing, so its command
// line is the one a process built before tiers accepts.
func storageTierArguments(tier storagetier.Tier) []string {
	if tier.NetworkLatency() == storagetier.Standard.NetworkLatency() {
		return nil
	}
	return []string{"--network-latency-ms", strconv.FormatInt(tier.NetworkLatency().Milliseconds(), 10)}
}
