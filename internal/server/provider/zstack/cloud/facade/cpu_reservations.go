package facade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

type CPUReservations struct{ client *apiclient.APIClient }

func NewCPUReservations(client *apiclient.APIClient) *CPUReservations {
	return &CPUReservations{client: client}
}

func (q *CPUReservations) Get(ctx context.Context, hostID string) (inventory.CPUReservation, error) {
	value, available, err := readCPUReservation(ctx, q.client, hostID)
	return inventory.CPUReservation{Value: value, HostOverrideAvailable: available}, err
}

type globalConfigInventory struct {
	Name         string `json:"name"`
	Category     string `json:"category"`
	Value        string `json:"value"`
	DefaultValue string `json:"defaultValue"`
}

func IsMissingResourceConfig(err error) bool {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Kind != apiclient.ErrorAuthentication &&
		(apiErr.ProtocolFailure == apiclient.ResourceConfigUnbound || apiErr.StatusCode == http.StatusNotFound || apiErr.Code == "20007" || apiErr.DiagnosticCode == "20007")
}

// readGlobalConfig reads a persisted global configuration value. The q
// filters are intentionally repeated: this is the query form supported by
// ZStack's REST API. A missing inventory means that the caller should use its
// own compatibility default.
func readGlobalConfig(ctx context.Context, c *apiclient.APIClient, category, name string) (string, bool, error) {
	var reply struct {
		Inventories []globalConfigInventory `json:"inventories"`
	}
	path := "/global-configurations?q=category=" + url.QueryEscape(category) + "&q=name=" + url.QueryEscape(name)
	if err := c.Get(ctx, path, &reply); err != nil {
		if IsMissingResourceConfig(err) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, inventory := range reply.Inventories {
		if inventory.Category != category || inventory.Name != name {
			continue
		}
		value := strings.TrimSpace(inventory.Value)
		if value == "" {
			value = strings.TrimSpace(inventory.DefaultValue)
		}
		return value, true, nil
	}
	return "", false, nil
}

// readCPUReservation prefers a host ResourceConfig override. When the host
// does not expose that binding, or exposes it without an override, the
// kvm.reservedCpu global configuration is the effective baseline.
func readCPUReservation(ctx context.Context, c *apiclient.APIClient, hostID string) (value int64, resourceConfigAvailable bool, err error) {
	var config struct {
		Value *string `json:"value"`
	}
	err = c.Get(ctx, "/resource-configurations/"+url.PathEscape(hostID)+"/kvm/reservedCpu", &config)
	if err != nil && !IsMissingResourceConfig(err) {
		return 0, false, err
	}
	resourceConfigAvailable = err == nil
	if config.Value != nil {
		value, parseErr := strconv.ParseInt(strings.TrimSpace(*config.Value), 10, 64)
		if parseErr != nil || value < 0 {
			return 0, resourceConfigAvailable, fmt.Errorf("zstack: invalid CPU reservation")
		}
		return value, resourceConfigAvailable, nil
	}
	globalValue, _, globalErr := readGlobalConfig(ctx, c, "kvm", "reservedCpu")
	if globalErr != nil {
		return 0, resourceConfigAvailable, fmt.Errorf("zstack: global CPU reservation query failed: %w", globalErr)
	}
	if globalValue == "" {
		return 0, resourceConfigAvailable, nil
	}
	value, parseErr := strconv.ParseInt(globalValue, 10, 64)
	if parseErr != nil || value < 0 {
		return 0, resourceConfigAvailable, fmt.Errorf("zstack: invalid global CPU reservation")
	}
	return value, resourceConfigAvailable, nil
}
