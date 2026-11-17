package compshare

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// listPageSize is the UCloud-style maximum Limit for one listing page.
const listPageSize = 100

func powerFromState(state string) provider.PowerState {
	switch strings.ToLower(state) {
	case "running":
		return provider.PowerRunning
	case "stopped":
		return provider.PowerStopped
	default:
		return provider.PowerUnknown
	}
}

// ListInstances pages through the account's instances. The result must be
// complete because sync removes rows for instances it does not list, so a
// listing that stops short of TotalCount or repeats an instance (an ignored
// Offset) is an error rather than a shorter list.
func (d *Driver) ListInstances(ctx context.Context, creds provider.Credentials) ([]provider.Instance, error) {
	out := []provider.Instance{}
	seen := map[string]bool{}
	for offset := 0; ; {
		resp, err := d.call(ctx, creds, map[string]string{
			"Action": listAction,
			"Offset": strconv.Itoa(offset),
			"Limit":  strconv.Itoa(listPageSize),
		})
		if err != nil {
			return nil, err
		}
		if len(resp.UHostSet) == 0 {
			if offset < resp.TotalCount {
				return nil, fmt.Errorf("compshare: instance listing ended at %d of %d", offset, resp.TotalCount)
			}
			return out, nil
		}
		for _, h := range resp.UHostSet {
			if seen[h.UHostId] {
				return nil, fmt.Errorf("compshare: instance listing repeated %s at offset %d", h.UHostId, offset)
			}
			seen[h.UHostId] = true
			out = append(out, mapInstance(h))
		}
		offset += len(resp.UHostSet)
		if resp.TotalCount > 0 {
			if offset >= resp.TotalCount {
				return out, nil
			}
		} else if len(resp.UHostSet) < listPageSize {
			return out, nil
		}
	}
}

func mapInstance(h ucloudInstance) provider.Instance {
	loginHost, port, user := parseSSHLoginCommand(h.SshLoginCommand)
	inst := provider.Instance{ExternalID: h.UHostId, Name: h.Name, GPUModel: h.GpuType, GPUCount: h.GPU,
		Power: powerFromState(h.State), PricePerHr: h.InstancePrice, Region: h.Region, Zone: h.Zone,
		SSHPort: port, SSHUser: firstNonEmpty(user, "root"), Kind: kindForInstance(h.InstanceType, h.MachineType)}
	for _, ip := range h.IPSet {
		if strings.EqualFold(ip.Type, "Private") && ip.IP != "" && inst.PrivateIP == "" {
			inst.PrivateIP = ip.IP
		}
	}
	inst.PublicIP = preferredManagementSSHHost(loginHost, h.IPSet)
	if inst.PublicIP == "" && len(h.IPSet) > 0 {
		inst.PublicIP = h.IPSet[0].IP
	}
	if inst.SSHPort == 0 {
		inst.SSHPort = 22
	}
	if inst.PublicIP != "" {
		inst.SSHConnections = []provider.SSHConnection{{ID: "direct", Type: "direct", Label: provider.SSHConnectionLabel("direct"), Host: inst.PublicIP, Port: inst.SSHPort, User: inst.SSHUser, InstallSupported: true}}
		provider.SetPreferredSSHConnection(&inst)
	}
	return inst
}

// kindFor maps the product/machine type to a runtime hint. compshare offers
// both VM and container products; default to "vm".
func kindFor(machineType string) string {
	return kindForInstance("", machineType)
}

func kindForInstance(instanceType, machineType string) string {
	if strings.Contains(strings.ToLower(instanceType), "container") || strings.Contains(strings.ToLower(machineType), "container") {
		return "container"
	}
	return "vm"
}

// GetInstance refreshes one instance and discovers its actual region/zone.
func (d *Driver) GetInstance(ctx context.Context, creds provider.Credentials, externalID string) (provider.Instance, error) {
	resp, err := d.call(ctx, creds, map[string]string{"Action": listAction, "UHostIds.0": externalID, "Limit": "1"})
	if err != nil {
		return provider.Instance{}, err
	}
	for _, h := range resp.UHostSet {
		if h.UHostId == externalID {
			return mapInstance(h), nil
		}
	}
	return provider.Instance{}, fmt.Errorf("%w: %s", errInstanceNotFound, externalID)
}

func (d *Driver) PowerState(ctx context.Context, creds provider.Credentials, externalID string) (provider.PowerState, error) {
	inst, err := d.GetInstance(ctx, creds, externalID)
	if err != nil {
		return provider.PowerUnknown, err
	}
	return inst.Power, nil
}

// SSHAccess resolves the generated password and SSH endpoint returned by the
// DescribeCompShareInstance API. Password is intentionally not placed on the
// public provider.Instance DTO.
func (d *Driver) SSHAccess(ctx context.Context, creds provider.Credentials, externalID string) (host string, port int, user, password string, err error) {
	resp, err := d.call(ctx, creds, map[string]string{"Action": listAction, "UHostIds.0": externalID, "Limit": "1"})
	if err != nil {
		return "", 0, "", "", err
	}
	for _, instance := range resp.UHostSet {
		if instance.UHostId != externalID {
			continue
		}
		loginHost, loginPort, loginUser := parseSSHLoginCommand(instance.SshLoginCommand)
		host = preferredManagementSSHHost(loginHost, instance.IPSet)
		port, user = loginPort, loginUser
		if port == 0 {
			port = 22
		}
		user = firstNonEmpty(user, "root")
		decoded, decodeErr := base64.StdEncoding.DecodeString(instance.Password)
		if decodeErr != nil {
			return "", 0, "", "", fmt.Errorf("compshare: decode instance password: %w", decodeErr)
		}
		return host, port, user, string(decoded), nil
	}
	return "", 0, "", "", fmt.Errorf("compshare: instance %s not found", externalID)
}

// InstancePassword is the root password CompShare generated for the
// instance. CompShare instances only offer password login, so the server
// logs in with it once and authorizes the platform key.
func (d *Driver) InstancePassword(ctx context.Context, creds provider.Credentials, externalID string) (string, error) {
	_, _, _, password, err := d.SSHAccess(ctx, creds, externalID)
	return password, err
}

// preferredManagementSSHHost keeps the management-plane SSH endpoint separate
// from the private address advertised for node-to-node traffic. CompShare may
// emit a private address in SshLoginCommand even when IPSet also contains a
// public address. Such a command works only from a tenant-routed network and is
// unreachable from ordinary GitLab runners and user workstations.
func preferredManagementSSHHost(loginHost string, addresses []compshareIP) string {
	loginHost = strings.TrimSpace(loginHost)
	if loginHost != "" && !isPrivateOrLocalIP(loginHost) {
		return loginHost
	}
	for _, address := range addresses {
		if strings.EqualFold(address.Type, "Private") || strings.TrimSpace(address.IP) == "" {
			continue
		}
		return strings.TrimSpace(address.IP)
	}
	return loginHost
}

func isPrivateOrLocalIP(host string) bool {
	ip := net.ParseIP(strings.Trim(strings.TrimSpace(host), "[]"))
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}

var sshLoginPattern = regexp.MustCompile(`^ssh(?:\s+-p\s+(\d+))?\s+([^@\s]+)@([^\s]+)(?:\s+-p\s+(\d+))?$`)

func parseSSHLoginCommand(command string) (host string, port int, user string) {
	match := sshLoginPattern.FindStringSubmatch(strings.TrimSpace(command))
	if len(match) == 0 {
		return "", 0, ""
	}
	user, host = match[2], match[3]
	rawPort := firstNonEmpty(match[1], match[4])
	port, _ = strconv.Atoi(rawPort)
	return host, port, user
}
