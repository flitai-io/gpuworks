package recovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// syncTTL is how long one provider listing serves every node of that
// provider in a recovery round, so a round with many offline nodes on one
// account lists it once.
const syncTTL = 30 * time.Second

type cachedSync struct {
	at        time.Time
	instances []provider.Instance
	err       error
}

// syncCache memoizes ProviderSyncer.Sync per provider for syncTTL.
type syncCache struct {
	mu      sync.Mutex
	entries map[uint]cachedSync
}

func (r *Recoverer) syncProvider(ctx context.Context, providerID uint) ([]provider.Instance, error) {
	r.syncs.mu.Lock()
	defer r.syncs.mu.Unlock()
	if r.syncs.entries == nil {
		r.syncs.entries = map[uint]cachedSync{}
	}
	if entry, ok := r.syncs.entries[providerID]; ok && r.now().Sub(entry.at) < syncTTL {
		return entry.instances, entry.err
	}
	instances, err := r.providers.Sync(ctx, providerID)
	r.syncs.entries[providerID] = cachedSync{at: r.now(), instances: instances, err: err}
	return instances, err
}

// refreshProviderEndpoints rereads, for a node on a provider instance, what a
// migration or a configuration change of that instance may have renumbered
// (RunPod renumbers every mapping of a pod whose configuration is edited; a
// plain restart keeps them): the SSH endpoint recovery
// logs in through and, for an agent the server dials, the address its
// listen port is mapped to. It reports whether that address changed: the
// server then dials it again before recovery logs in to the host.
func (r *Recoverer) refreshProviderEndpoints(ctx context.Context, node *store.Node) (bool, error) {
	if r.providers == nil {
		return false, nil
	}
	linked, err := r.store.ProviderInstanceForNode(node.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if node.HasSSHCredential {
		if err := r.refreshSSHEndpoint(ctx, node, linked); err != nil {
			r.log.Warn("refresh provider SSH endpoint failed", "node", node.UUID, "err", err)
		}
	}
	if node.ConnectMode != "server_dial" {
		return false, nil
	}
	address, err := r.providers.ServerDialAddress(ctx, linked.ProviderID, linked.ExternalID, node.ServerDialListen)
	if errors.Is(err, provider.ErrUnsupported) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the agent's mapped address: %w", err)
	}
	if strings.TrimSpace(address) == "" || address == node.ServerDialAddress {
		return false, nil
	}
	if err := r.store.SetNodeServerDialAddress(node.UUID, address); err != nil {
		return false, err
	}
	r.log.Info("provider moved a server_dial agent; dialing its new address", "node", node.UUID, "from", node.ServerDialAddress, "to", address)
	node.ServerDialAddress = address
	if r.hub != nil {
		r.hub.ResetServerDialBackoff(node.UUID)
	}
	return true, nil
}

// refreshSSHEndpoint points the node's SSH credential at the instance's
// current endpoint when the stored one is no longer among the instance's
// SSH endpoints (a renumbered port); the user and secrets stay.
func (r *Recoverer) refreshSSHEndpoint(ctx context.Context, node *store.Node, linked *store.ProviderInstance) error {
	instances, err := r.syncProvider(ctx, linked.ProviderID)
	if err != nil {
		return err
	}
	var inst *provider.Instance
	for i := range instances {
		if instances[i].ExternalID == linked.ExternalID {
			inst = &instances[i]
			break
		}
	}
	if inst == nil {
		return nil
	}
	// The listing may omit the connection data (a RunPod pod's list carries
	// no direct SSH endpoint at all), so what the provider says about this
	// instance itself decides where it is reached.
	if detail, err := r.providers.InstanceDetail(ctx, linked.ProviderID, linked.ExternalID); err == nil {
		if _, err := provider.SSHInstallCandidates(detail, "auto"); err == nil {
			inst = &detail
		}
	} else if !errors.Is(err, provider.ErrUnsupported) {
		r.log.Warn("read provider instance detail failed", "node", node.UUID, "err", err)
	}
	credential, secrets, err := r.store.GetNodeSSHCredential(node.UUID)
	if err != nil {
		return err
	}
	for _, connection := range inst.SSHConnections {
		if provider.IsSSHConnection(connection) && connection.Host == credential.Host && connection.Port == credential.Port {
			return nil
		}
	}
	candidates, err := provider.SSHInstallCandidates(*inst, "auto")
	if err != nil {
		return nil //nolint:nilerr // Optional refresh retains the existing credential endpoint when no candidates are available.
	}
	for _, connection := range candidates {
		if !provider.IsSSHConnection(connection) || strings.TrimSpace(connection.Host) == "" || connection.Port == 0 {
			continue
		}
		if _, err := r.store.SetNodeSSHCredential(node.UUID, connection.Host, connection.Port, credential.User, secrets); err != nil {
			return err
		}
		r.log.Info("provider renumbered a node's SSH endpoint", "node", node.UUID, "from", fmt.Sprintf("%s:%d", credential.Host, credential.Port), "to", fmt.Sprintf("%s:%d", connection.Host, connection.Port))
		return nil
	}
	return nil
}
