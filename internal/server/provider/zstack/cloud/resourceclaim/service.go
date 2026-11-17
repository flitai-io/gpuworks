// Package resourceclaim manages ZStack Cloud GPU ownership for inference instances.
package resourceclaim

import (
	"context"
	"fmt"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

// Service owns Cloud resource leases and the inference-node lifecycle hooks.
type Service struct {
	// Serializes observation and confirmed writes within this application.
	resourceMu sync.Mutex
	store      *store.Store
	cloudStore *cloudstore.Store
	providers  *provider.Service
}

// New binds an application-scoped lease service to its store and provider drivers.
func New(st *store.Store, providers *provider.Service) *Service {
	return &Service{store: st, cloudStore: st.CloudLedger(), providers: providers}
}

func (s *Service) driverCreds(providerID uint) (provider.Driver, provider.Credentials, error) {
	if s.providers == nil {
		return nil, provider.Credentials{}, fmt.Errorf("provider service is unavailable")
	}
	return s.providers.DriverCredentials(providerID)
}

func (s *Service) applyResourceMutation(ctx context.Context, providerID uint, mutation ResourceMutation) error {
	driver, creds, err := s.driverCreds(providerID)
	if err != nil {
		return err
	}
	mutator, ok := driver.(ResourceMutator)
	if !ok {
		return provider.ErrUnsupported
	}
	return mutator.ApplyResourceMutation(ctx, creds, mutation)
}

var _ provider.InferenceNodeLifecycleHandler = (*Service)(nil)
