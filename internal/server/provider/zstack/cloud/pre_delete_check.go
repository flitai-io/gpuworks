package cloud

import (
	"context"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func (d *Driver) PreDeleteCheck(ctx context.Context, providerID uint) error {
	if d.store == nil {
		return fmt.Errorf("Cloud resource store is not configured")
	}
	ledger := d.store.CloudLedger()
	linked, err := ledger.HasProviderNodes(ctx, providerID)
	if err != nil {
		return err
	}
	if linked {
		return fmt.Errorf("%w: remove the linked Cloud compute nodes first", provider.ErrProviderInUse)
	}
	inUse, err := ledger.HasUnreleasedProviderLeases(ctx, providerID)
	if err != nil {
		return err
	}
	if inUse {
		return fmt.Errorf("%w: confirm all Cloud resource leases are released first", provider.ErrProviderInUse)
	}
	return nil
}

var _ provider.PreDeleteChecker = (*Driver)(nil)
