package compshare

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// ResourceCapacityRequest describes the image and storage shape used when
// asking CompShare which GPU layouts are currently available. The API returns
// every supported GPU count/CPU/memory combination without creating a VM.
type ResourceCapacityRequest struct {
	GPUModel    string
	DiskGB      int
	Image       string
	Region      string
	Zone        string
	ChargeType  string
	CPUPlatform string
}

// ResourceCapacitySpec is one shape returned by CheckResourceCapacity.
type ResourceCapacitySpec struct {
	GPU            int
	CPU            int
	MemoryGB       int
	ResourceEnough bool
}

// CheckResourceCapacity queries the official CompShare capacity endpoint. It
// is intentionally allocation-free and can be used immediately before a
// live test to select a shape that is actually available.
func (d *Driver) CheckResourceCapacity(ctx context.Context, creds provider.Credentials, req ResourceCapacityRequest) ([]ResourceCapacitySpec, error) {
	if strings.TrimSpace(req.GPUModel) == "" {
		return nil, fmt.Errorf("compshare: GPU model is required for capacity check")
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		return nil, fmt.Errorf("compshare: CompShare image id is required for capacity check")
	}
	diskGB := req.DiskGB
	if diskGB <= 0 {
		diskGB = 100
	}
	region := firstNonEmpty(req.Region, defaultRegion)
	zone := firstNonEmpty(req.Zone, defaultZone)
	chargeType := firstNonEmpty(req.ChargeType, "Postpay")
	cpuPlatform := firstNonEmpty(req.CPUPlatform, "Auto")
	params := capacityParams(resourceCapacityParams{
		gpuModel: req.GPUModel, region: region, zone: zone, image: image,
		diskGB: diskGB, chargeType: chargeType, cpuPlatform: cpuPlatform,
	})
	var resp *ucloudResp
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = d.call(ctx, creds, params)
		if err == nil || !isCreateTransientError(err) || attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(2*(attempt+1)) * time.Second):
		}
	}
	if err != nil {
		return nil, fmt.Errorf("compshare: check resource capacity: %w", err)
	}
	out := make([]ResourceCapacitySpec, 0, len(resp.Specs))
	for _, spec := range resp.Specs {
		out = append(out, ResourceCapacitySpec(spec))
	}
	return out, nil
}
