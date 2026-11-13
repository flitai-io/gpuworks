package provider

import (
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Record stores an account and the instances a sync would have listed for
// it, without asking the vendor: the experience environment's accounts are
// for showing, and it refuses every operation on them.
func (s *Service) Record(kind, name string, creds Credentials, insts []Instance) (*store.Provider, error) {
	if _, ok := s.driver(kind); !ok {
		return nil, fmt.Errorf("unknown provider kind: %s", kind)
	}
	p, err := s.create(kind, name, creds)
	if err != nil {
		return nil, err
	}
	rows, err := instanceRows(p.ID, insts)
	if err != nil {
		return nil, err
	}
	return p, s.store.SyncProviderInstances(p.ID, rows)
}

// create stores a provider with its credentials encrypted.
func (s *Service) create(kind, name string, creds Credentials) (*store.Provider, error) {
	raw, err := json.Marshal(creds)
	if err != nil {
		return nil, err
	}
	cipher, err := s.box.Encrypt(raw)
	if err != nil {
		return nil, err
	}
	return s.store.CreateProvider(&store.Provider{Name: name, Kind: kind, CredsCipher: cipher, State: "valid"})
}

// instanceRows are a listing's stored rows.
func instanceRows(providerID uint, insts []Instance) ([]store.ProviderInstance, error) {
	rows := make([]store.ProviderInstance, 0, len(insts))
	for _, in := range insts {
		connections, err := json.Marshal(in.SSHConnections)
		if err != nil {
			return nil, fmt.Errorf("encode SSH connections for instance %s: %w", in.ExternalID, err)
		}
		rows = append(rows, store.ProviderInstance{
			ProviderID: providerID, ExternalID: in.ExternalID, Name: in.Name,
			GPUModel: in.GPUModel, GPUCount: in.GPUCount, Power: string(in.Power),
			PricePerHr: in.PricePerHr, Region: in.Region, PublicIP: in.PublicIP,
			SSHPort: in.SSHPort, SSHUser: in.SSHUser, SSHConnections: string(connections), Kind: in.Kind,
			ManagedCluster: in.Cluster,
		})
	}
	return rows, nil
}
