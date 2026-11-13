package vast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// A Vast instance is reached only through the TCP ports it was created with,
// each mapped to a host port on its public address (docker -p). The vastai
// base images map a few beyond the ones their own services hold (10100 and
// 10200 on vastai/base-image), and the server dials the agent on one of
// those. The ports are fixed at creation: an instance without a free one
// cannot be dialed by the server.

// tcpMappings are the instance's mapped TCP ports: container port → host
// port on its public address.
func (in *instance) tcpMappings() map[int]int {
	out := map[int]int{}
	if len(in.Ports) == 0 {
		return out
	}
	var mappings map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal(in.Ports, &mappings); err != nil {
		return out
	}
	for key, hosts := range mappings {
		port, proto, ok := strings.Cut(key, "/")
		if !ok || proto != "tcp" {
			continue
		}
		container, err := strconv.Atoi(port)
		if err != nil || container < 1 || container > 65535 {
			continue
		}
		for _, host := range hosts {
			if public, err := strconv.Atoi(host.HostPort); err == nil && public > 0 && public <= 65535 {
				out[container] = public
				break
			}
		}
	}
	return out
}

// instanceByID finds one instance; Vast's API lists them all.
func (d *Driver) instanceByID(ctx context.Context, creds provider.Credentials, externalID string) (instance, error) {
	resp, err := d.do(ctx, creds, http.MethodGet, "/instances/")
	if err != nil {
		return instance{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return instance{}, fmt.Errorf("vast: list instances: status %d", resp.StatusCode)
	}
	insts, err := decodeInstances(resp.Body)
	if err != nil {
		return instance{}, fmt.Errorf("vast: decode instances: %w", err)
	}
	for i := range insts {
		if insts[i].ID.String() == externalID {
			return insts[i], nil
		}
	}
	return instance{}, fmt.Errorf("vast: instance %q not found", externalID)
}

// ServerDialAddressFor implements provider.ServerDialPortMapper: the public
// address the container port of listen is mapped to now.
func (d *Driver) ServerDialAddressFor(ctx context.Context, creds provider.Credentials, externalID, listen string) (string, error) {
	in, err := d.instanceByID(ctx, creds, externalID)
	if err != nil {
		return "", err
	}
	port := provider.PortOf(listen)
	public, ok := in.tcpMappings()[port]
	if !ok || in.PublicIPAddr == "" {
		return "", fmt.Errorf("vast: instance %s no longer maps container port %d", externalID, port)
	}
	return net.JoinHostPort(in.PublicIPAddr, strconv.Itoa(public)), nil
}

// installServerDial puts a server-dialed agent on a mapped port nothing on
// the instance listens on and uploads it over SSH: the instance may not
// reach the server, so it downloads nothing.
func (d *Driver) installServerDial(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	user := firstNonEmpty(req.Instance.SSHUser, "root")
	port := req.Instance.SSHPort
	if port == 0 {
		port = 22
	}
	auth := req.SSHAuth()
	if err := auth.Validate(); err != nil {
		return provider.InstallReport{}, fmt.Errorf("vast: %w", err)
	}
	listen, address := req.ServerDialListen, req.ServerDialAddress
	var chosen provider.InstallCheck
	// The platform key was attached a moment ago: wait for Vast to apply it
	// before the installer logs in.
	client, _, err := d.dialFresh(ctx, user, req.Instance.PublicIP, port, auth)
	if err != nil {
		return provider.InstallReport{}, provider.RetryableSSHConnectionError("vast: ssh "+net.JoinHostPort(req.Instance.PublicIP, strconv.Itoa(port)), err)
	}
	defer client.Close()
	if strings.TrimSpace(address) == "" {
		in, err := d.instanceByID(ctx, creds, req.Instance.ExternalID)
		if err != nil {
			return provider.InstallReport{}, err
		}
		listening, err := provision.ListeningPorts(ctx, client)
		if err != nil {
			return provider.InstallReport{}, fmt.Errorf("vast: %w", err)
		}
		mapped := in.tcpMappings()
		container, ok := provider.ChooseMappedPort(mapped, listening, provider.PortOf(listen))
		if !ok || in.PublicIPAddr == "" {
			return provider.InstallReport{}, fmt.Errorf("vast: instance %s maps no free TCP port the server could dial the agent on (its ports are fixed when it is created: rent one whose template maps a spare port, or let the agent connect to the server)", req.Instance.ExternalID)
		}
		listen = provider.ListenOnPort(container)
		address = net.JoinHostPort(in.PublicIPAddr, strconv.Itoa(mapped[container]))
		chosen = provider.InstallCheck{Name: "Server-dial port", Status: "ok", Detail: fmt.Sprintf("container port %d, mapped to %s", container, address)}
	}
	installer := provision.NewSSHInstaller(provision.AgentBinaryReader(req.ReadAgentBinary))
	report, err := installer.InstallSSH(ctx, provision.SSHInstallRequest{
		Target: req.Instance.PublicIP, User: user, Port: port,
		ProviderKind: "vast", InstanceKind: req.InstanceKind,
		Token: req.Token, ConnectMode: "server_dial",
		ServerDialListen: listen, ServerDialAddress: address,
		InstallDir: req.InstallDir, NoSystemd: req.NoSystemd,
		SSHAuth: auth,
	})
	if err != nil {
		var connectionErr *provision.SSHConnectionError
		if errors.As(err, &connectionErr) {
			return provider.InstallReport{}, provider.RetryableSSHConnectionError("vast: SSH connection", connectionErr)
		}
		return provider.InstallReport{}, fmt.Errorf("vast: install agent: %w", err)
	}
	out := provider.ReportFromSSHInstall(report)
	if chosen.Name != "" {
		out.Checks = append([]provider.InstallCheck{chosen}, out.Checks...)
	}
	out.ServerDialListen, out.ServerDialAddress = listen, address
	return out, nil
}

// keyAttachWait bounds the retries of the first login: a key attached to the
// instance through the API a moment ago is not accepted at once (observed
// 2026-10-19: the first login failed, one 20 seconds later succeeded).
const keyAttachWait = 45 * time.Second

// dialFresh logs in to an instance whose platform key may just have been
// attached, trying again for keyAttachWait.
func (d *Driver) dialFresh(ctx context.Context, user, host string, port int, auth provision.SSHAuth) (*ssh.Client, provision.AuthOutcome, error) {
	wait := keyAttachWait
	if d.keyAttachWait > 0 {
		wait = d.keyAttachWait
	}
	deadline := time.Now().Add(wait)
	for {
		client, outcome, err := provision.DialSSH(ctx, user, host, port, auth, 10*time.Second, nil)
		if err == nil {
			return client, outcome, nil
		}
		if time.Now().After(deadline) {
			return nil, outcome, err
		}
		select {
		case <-ctx.Done():
			return nil, outcome, err
		case <-time.After(wait / 9):
		}
	}
}
