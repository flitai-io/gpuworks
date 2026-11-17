// Package facade provides uniform Cloud interfaces, hiding capability and protocol
// differences. Business workflows stay in the caller; transport uses apiclient.
package facade

import (
	"context"
	"errors"
	"sync/atomic"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

// GPUQueries binds protocol selection to one environment and operation.
type GPUQueries struct {
	device GPUDeviceQuery
	pci    PCIGPUQuery
	usePCI atomic.Bool
}

func NewGPUQuery(client *apiclient.APIClient) *GPUQueries {
	return &GPUQueries{device: GPUDeviceQuery{client: client}, pci: PCIGPUQuery{client: client}}
}

func (q *GPUQueries) List(ctx context.Context) ([]inventory.GPUDevice, error) {
	return queryGPUs(q, func() ([]inventory.GPUDevice, error) { return q.device.List(ctx) },
		func() ([]inventory.GPUDevice, error) { return q.pci.List(ctx) })
}

func (q *GPUQueries) Get(ctx context.Context, uuid string) (inventory.GPUDevice, error) {
	return queryGPUs(q, func() (inventory.GPUDevice, error) { return q.device.Get(ctx, uuid) },
		func() (inventory.GPUDevice, error) { return q.pci.Get(ctx, uuid) })
}

func queryGPUs[T any](q *GPUQueries, current, pci func() (T, error)) (T, error) {
	if q.usePCI.Load() {
		return pci()
	}
	result, err := current()
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.ProtocolFailure != apiclient.APIRouteMissing {
		return result, err
	}
	q.usePCI.Store(true)
	return pci()
}
