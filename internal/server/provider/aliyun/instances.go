package aliyun

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// regionConcurrency bounds the regions listed at once: enough that a sync
// over every region takes seconds, few enough to stay clear of per-account
// API throttling.
const regionConcurrency = 6

// apiInstance is the subset of DescribeInstances' Instance the driver maps:
// https://help.aliyun.com/zh/ecs/developer-reference/api-ecs-2014-05-26-describeinstances
type apiInstance struct {
	InstanceID      string `json:"InstanceId"`
	InstanceName    string `json:"InstanceName"`
	RegionID        string `json:"RegionId"`
	ZoneID          string `json:"ZoneId"`
	Status          string `json:"Status"`
	OSType          string `json:"OSType"`
	GPUAmount       int    `json:"GPUAmount"`
	GPUSpec         string `json:"GPUSpec"`
	PublicIPAddress struct {
		IPAddress []string `json:"IpAddress"`
	} `json:"PublicIpAddress"`
	EIPAddress struct {
		IPAddress string `json:"IpAddress"`
	} `json:"EipAddress"`
	VPCAttributes struct {
		PrivateIPAddress struct {
			IPAddress []string `json:"IpAddress"`
		} `json:"PrivateIpAddress"`
	} `json:"VpcAttributes"`
	InnerIPAddress struct {
		IPAddress []string `json:"IpAddress"`
	} `json:"InnerIpAddress"`
	SecurityGroupIDs struct {
		SecurityGroupID []string `json:"SecurityGroupId"`
	} `json:"SecurityGroupIds"`
	Tags struct {
		Tag []struct {
			TagKey   string `json:"TagKey"`
			TagValue string `json:"TagValue"`
		} `json:"Tag"`
	} `json:"Tags"`
}

// tag returns the value of one of the instance's tags.
func (i apiInstance) tag(key string) string {
	for _, t := range i.Tags.Tag {
		if t.TagKey == key {
			return t.TagValue
		}
	}
	return ""
}

func (i apiInstance) publicIP() string {
	if len(i.PublicIPAddress.IPAddress) > 0 {
		return i.PublicIPAddress.IPAddress[0]
	}
	return i.EIPAddress.IPAddress
}

func (i apiInstance) privateIP() string {
	if len(i.VPCAttributes.PrivateIPAddress.IPAddress) > 0 {
		return i.VPCAttributes.PrivateIPAddress.IPAddress[0]
	}
	if len(i.InnerIPAddress.IPAddress) > 0 {
		return i.InnerIPAddress.IPAddress[0]
	}
	return ""
}

func mapPower(status string) provider.PowerState {
	switch status {
	case "Running":
		return provider.PowerRunning
	case "Stopped":
		return provider.PowerStopped
	default: // Pending, Starting, Stopping
		return provider.PowerUnknown
	}
}

// regions returns the regions in scope: the account's own list, or the
// operator's subset from the "regions" credential field.
func (d *Driver) regions(ctx context.Context, creds provider.Credentials) ([]string, error) {
	var out struct {
		Regions struct {
			Region []struct {
				RegionID string `json:"RegionId"`
			} `json:"Region"`
		} `json:"Regions"`
	}
	if err := d.api.call(ctx, creds, ecsProduct, "", "DescribeRegions", nil, &out); err != nil {
		return nil, err
	}
	available := map[string]bool{}
	var all []string
	for _, r := range out.Regions.Region {
		available[r.RegionID] = true
		all = append(all, r.RegionID)
	}
	chosen := selectedRegions(creds)
	if len(chosen) == 0 {
		return all, nil
	}
	for _, region := range chosen {
		if !available[region] {
			return nil, fmt.Errorf("aliyun: region %q is not available to this account", region)
		}
	}
	return chosen, nil
}

func selectedRegions(creds provider.Credentials) []string {
	if creds.Extra == nil {
		return nil
	}
	var regions []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(creds.Extra["regions"], func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" && !seen[part] {
			seen[part] = true
			regions = append(regions, part)
		}
	}
	return regions
}

// forEachRegion runs fn over regions with bounded concurrency and gathers the
// results. One region failing fails the whole call: a sync that silently
// skipped a region would drop that region's instances from the inventory.
func forEachRegion[T any](ctx context.Context, regions []string, fn func(context.Context, string) ([]T, error)) ([]T, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		results  = make([][]T, len(regions))
		firstErr error
		wg       sync.WaitGroup
		slots    = make(chan struct{}, regionConcurrency)
	)
	for i, region := range regions {
		wg.Add(1)
		go func(i int, region string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			items, err := fn(ctx, region)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("region %s: %w", region, err)
					cancel()
				}
				return
			}
			results[i] = items
		}(i, region)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	var all []T
	for _, items := range results {
		all = append(all, items...)
	}
	return all, nil
}

// describeInstances pages through one region with NextToken (PageNumber is
// throttled by the API).
func (d *Driver) describeInstances(ctx context.Context, creds provider.Credentials, region string, filter map[string]string) ([]apiInstance, error) {
	var all []apiInstance
	token := ""
	for page := 0; page < 1000; page++ {
		params := map[string]string{"MaxResults": "100"}
		for k, v := range filter {
			params[k] = v
		}
		if token != "" {
			params["NextToken"] = token
		}
		var out struct {
			Instances struct {
				Instance []apiInstance `json:"Instance"`
			} `json:"Instances"`
			NextToken string `json:"NextToken"`
		}
		if err := d.api.call(ctx, creds, ecsProduct, region, "DescribeInstances", params, &out); err != nil {
			return nil, err
		}
		for _, inst := range out.Instances.Instance {
			if inst.RegionID == "" {
				inst.RegionID = region
			}
			all = append(all, inst)
		}
		if out.NextToken == "" {
			return all, nil
		}
		token = out.NextToken
	}
	return nil, fmt.Errorf("aliyun: DescribeInstances in %s did not finish paging", region)
}

// assistantOnline returns which of the region's instances have Cloud
// Assistant online, 50 IDs per call (the API's limit).
func (d *Driver) assistantOnline(ctx context.Context, creds provider.Credentials, region string, ids []string) (map[string]bool, error) {
	online := map[string]bool{}
	for start := 0; start < len(ids); start += 50 {
		end := min(start+50, len(ids))
		params := map[string]string{"MaxResults": "50"}
		for i, id := range ids[start:end] {
			params["InstanceId."+strconv.Itoa(i+1)] = id
		}
		var out struct {
			Set struct {
				Status []struct {
					InstanceID string `json:"InstanceId"`
					Online     string `json:"CloudAssistantStatus"`
				} `json:"InstanceCloudAssistantStatus"`
			} `json:"InstanceCloudAssistantStatusSet"`
		}
		if err := d.api.call(ctx, creds, ecsProduct, region, "DescribeCloudAssistantStatus", params, &out); err != nil {
			return nil, err
		}
		for _, status := range out.Set.Status {
			online[status.InstanceID] = status.Online == "true"
		}
	}
	return online, nil
}

// ListInstances lists the GPU instances in the regions in scope: an account
// often holds many more CPU-only instances, which the agent cannot serve.
func (d *Driver) ListInstances(ctx context.Context, creds provider.Credentials) ([]provider.Instance, error) {
	regions, err := d.regions(ctx, creds)
	if err != nil {
		return nil, err
	}
	instances, err := forEachRegion(ctx, regions, func(ctx context.Context, region string) ([]provider.Instance, error) {
		all, err := d.describeInstances(ctx, creds, region, nil)
		if err != nil {
			return nil, err
		}
		raw := all[:0]
		for _, inst := range all {
			if inst.GPUAmount > 0 {
				raw = append(raw, inst)
			}
		}
		var running []string
		for _, inst := range raw {
			if inst.Status == "Running" && !strings.EqualFold(inst.OSType, "windows") {
				running = append(running, inst.InstanceID)
			}
		}
		online := map[string]bool{}
		if len(running) > 0 {
			if online, err = d.assistantOnline(ctx, creds, region, running); err != nil {
				return nil, fmt.Errorf("cloud assistant status: %w", err)
			}
		}
		out := make([]provider.Instance, 0, len(raw))
		for _, inst := range raw {
			out = append(out, mapInstance(inst, online[inst.InstanceID]))
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(instances, func(i, j int) bool {
		if instances[i].Region != instances[j].Region {
			return instances[i].Region < instances[j].Region
		}
		return instances[i].ExternalID < instances[j].ExternalID
	})
	return instances, nil
}

// mapInstance maps an ECS instance. Its install paths are Cloud Assistant
// when that is online, and SSH as root on the public address when there is
// one; Windows instances get neither (the agent runs on Linux).
func mapInstance(inst apiInstance, assistantOnline bool) provider.Instance {
	out := provider.Instance{
		ExternalID: inst.InstanceID,
		Name:       inst.InstanceName,
		GPUModel:   strings.TrimSpace(inst.GPUSpec),
		GPUCount:   inst.GPUAmount,
		Power:      mapPower(inst.Status),
		Region:     inst.RegionID,
		Zone:       inst.ZoneID,
		PublicIP:   inst.publicIP(),
		PrivateIP:  inst.privateIP(),
		Kind:       "vm",
		Cluster:    inst.tag(ackClusterTag),
	}
	if out.Name == "" {
		out.Name = inst.InstanceID
	}
	if strings.EqualFold(inst.OSType, "windows") {
		return out
	}
	if assistantOnline {
		out.SSHConnections = append(out.SSHConnections, provider.SSHConnection{
			ID: provider.ConnectionCloudAssistant, Type: provider.ConnectionCloudAssistant,
			Label: provider.SSHConnectionLabel(provider.ConnectionCloudAssistant), InstallSupported: true,
		})
	}
	if ip := out.PublicIP; ip != "" {
		out.SSHConnections = append(out.SSHConnections, provider.SSHConnection{
			ID: "direct", Type: "direct", Label: provider.SSHConnectionLabel("direct"),
			Host: ip, Port: 22, User: "root", InstallSupported: true,
		})
	}
	provider.SetPreferredSSHConnection(&out)
	return out
}
