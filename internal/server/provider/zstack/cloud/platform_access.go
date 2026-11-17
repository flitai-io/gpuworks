package cloud

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/facade"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

type GPUQuery interface {
	List(context.Context) ([]inventory.GPUDevice, error)
	Get(context.Context, string) (inventory.GPUDevice, error)
}

type CPUReservationQuery interface {
	Get(context.Context, string) (inventory.CPUReservation, error)
}

type platformAccess struct {
	gpus GPUQuery
	cpu  CPUReservationQuery
}

func newAccess(client *apiclient.APIClient) platformAccess {
	return platformAccess{gpus: facade.NewGPUQuery(client), cpu: facade.NewCPUReservations(client)}
}
