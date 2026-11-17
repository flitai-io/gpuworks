package compshare

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// A CompShare container instance is reached only through the ports it opens:
// each open TCP port is forwarded from a shared host
// (…podtcp.compshare.cn) to the container, SSH's included (container port
// 23). The server dials a server-dialed agent on one of them. When every
// open TCP port is taken, the install opens another with
// UpdateCompShareInstancePorts, which applies at once and does not restart
// the container (observed 2026-10-23). A VM has a public address, where the
// server dials the agent's listen port directly.

// openPorts is an instance's Ports: the container ports it opens.
type openPorts struct {
	HttpPorts []int `json:"HttpPorts"`
	TcpPorts  []int `json:"TcpPorts"`
}

// tcpForward is one entry of an instance's TcpForwards.
type tcpForward struct {
	ExternalHost string `json:"ExternalHost"`
	ExternalPort int    `json:"ExternalPort"`
	InternalPort int    `json:"InternalPort"`
}

func (f tcpForward) address() string {
	return net.JoinHostPort(f.ExternalHost, strconv.Itoa(f.ExternalPort))
}

// maxTCPPorts is CompShare's limit on a container's open TCP ports, SSH's
// included.
const maxTCPPorts = 10

// defaultAgentPort is the container port opened for the agent when the
// operator named none.
const defaultAgentPort = 3389

// forwardsByPort indexes usable forwards by container port.
func forwardsByPort(forwards []tcpForward) map[int]tcpForward {
	out := map[int]tcpForward{}
	for _, f := range forwards {
		if f.ExternalHost == "" || f.InternalPort < 1 || f.InternalPort > 65535 || f.ExternalPort < 1 || f.ExternalPort > 65535 {
			continue
		}
		out[f.InternalPort] = f
	}
	return out
}

// MapsServerDialPorts implements provider.InstancePortMapper: only a
// container's ports are forwarded.
func (d *Driver) MapsServerDialPorts(inst provider.Instance) bool { return inst.Kind == "container" }

func (d *Driver) describe(ctx context.Context, creds provider.Credentials, externalID string) (ucloudInstance, error) {
	resp, err := d.call(ctx, creds, map[string]string{"Action": listAction, "UHostIds.0": externalID, "Limit": "1"})
	if err != nil {
		return ucloudInstance{}, err
	}
	for _, h := range resp.UHostSet {
		if h.UHostId == externalID {
			return h, nil
		}
	}
	return ucloudInstance{}, fmt.Errorf("compshare: instance %s not found", externalID)
}

// ServerDialAddressFor implements provider.ServerDialPortMapper: where the
// container port of listen is forwarded from now. A VM is dialed at the
// address the install recorded.
func (d *Driver) ServerDialAddressFor(ctx context.Context, creds provider.Credentials, externalID, listen string) (string, error) {
	h, err := d.describe(ctx, creds, externalID)
	if err != nil {
		return "", err
	}
	if kindForInstance(h.InstanceType, h.MachineType) != "container" {
		return "", provider.ErrUnsupported
	}
	port := provider.PortOf(listen)
	f, ok := forwardsByPort(h.TcpForwards)[port]
	if !ok {
		return "", fmt.Errorf("compshare: instance %s no longer opens container port %d", externalID, port)
	}
	return f.address(), nil
}

// serverDialPort chooses the container port a server-dialed agent listens
// on and the address the server reaches it at: an open TCP port nothing
// holds, the operator's port first, else one it opens.
func (d *Driver) serverDialPort(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (string, string, provision.CheckResult, error) {
	h, err := d.describe(ctx, creds, req.Instance.ExternalID)
	if err != nil {
		return "", "", provision.CheckResult{}, err
	}
	sshPort := firstPort(req.Instance.SSHPort)
	client, _, err := provision.DialSSH(ctx, firstNonEmptyLocal(req.Instance.SSHUser, "root"), req.Instance.PublicIP, sshPort, req.SSHAuth(), 15*time.Second, d.hostKeys)
	if err != nil {
		return "", "", provision.CheckResult{}, provider.RetryableSSHConnectionError("compshare: ssh "+net.JoinHostPort(req.Instance.PublicIP, strconv.Itoa(sshPort)), err)
	}
	listening, err := provision.ListeningPorts(ctx, client)
	_ = client.Close()
	if err != nil {
		return "", "", provision.CheckResult{}, fmt.Errorf("compshare: %w", err)
	}
	forwards := forwardsByPort(h.TcpForwards)
	mapped := make(map[int]int, len(forwards))
	for port, f := range forwards {
		mapped[port] = f.ExternalPort
	}
	preferred := provider.PortOf(req.ServerDialListen)
	if preferred == 0 {
		preferred = defaultAgentPort
	}
	if port, ok := provider.ChooseMappedPort(mapped, listening, preferred); ok {
		address := forwards[port].address()
		return provider.ListenOnPort(port), address, provider.ServerDialPortCheck(port, address, false, false,
			fmt.Sprintf("container port %d, already open and forwarded from %s", port, address)), nil
	}
	port := preferred
	for port <= 65535 && (listening[port] || port == 22) {
		port++
	}
	if port > 65535 {
		return "", "", provision.CheckResult{}, fmt.Errorf("compshare: no free container port at or above %d for the agent", preferred)
	}
	f, err := d.openTCPPort(ctx, creds, h, port)
	if err != nil {
		return "", "", provision.CheckResult{}, err
	}
	return provider.ListenOnPort(port), f.address(), provider.ServerDialPortCheck(port, f.address(), true, false,
		fmt.Sprintf("opened container port %d on the instance, forwarded from %s", port, f.address())), nil
}

// openTCPPort adds port to the instance's open TCP ports. The API replaces
// the whole list, so it is sent with every port already open.
func (d *Driver) openTCPPort(ctx context.Context, creds provider.Credentials, h ucloudInstance, port int) (tcpForward, error) {
	if len(h.Ports.TcpPorts) >= maxTCPPorts {
		return tcpForward{}, fmt.Errorf("compshare: instance %s already opens %d TCP ports, the most CompShare allows, and none is free for the agent: close one in the console or let the agent connect to the server", h.UHostId, len(h.Ports.TcpPorts))
	}
	params := map[string]string{"Action": "UpdateCompShareInstancePorts", "Region": h.Region, "Zone": h.Zone, "UHostId": h.UHostId}
	for i, p := range h.Ports.HttpPorts {
		params["HttpPorts."+strconv.Itoa(i)] = strconv.Itoa(p)
	}
	for i, p := range h.Ports.TcpPorts {
		params["TcpPorts."+strconv.Itoa(i)] = strconv.Itoa(p)
	}
	params["TcpPorts."+strconv.Itoa(len(h.Ports.TcpPorts))] = strconv.Itoa(port)
	resp, err := d.call(ctx, creds, params)
	if err != nil {
		return tcpForward{}, fmt.Errorf("compshare: open TCP port %d for the agent: %w", port, err)
	}
	if f, ok := forwardsByPort(resp.TcpForwards)[port]; ok {
		return f, nil
	}
	again, err := d.describe(ctx, creds, h.UHostId)
	if err != nil {
		return tcpForward{}, err
	}
	if f, ok := forwardsByPort(again.TcpForwards)[port]; ok {
		return f, nil
	}
	return tcpForward{}, fmt.Errorf("compshare: opened TCP port %d but the instance does not forward it", port)
}
