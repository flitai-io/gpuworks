package runpod

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// A RunPod pod is reached only through the TCP ports it exposes, each mapped
// to a port on its public address. A pod may already expose a spare one;
// otherwise exposing another means editing the pod, which restarts its
// container (clearing the container disk) and renumbers every mapping,
// SSH's included (observed 2026-10-19). So that is done only when the
// operator agreed (InstallRequest.AllowInstanceRestart).

// defaultAgentPort is the container port exposed for the agent when the
// operator named none.
const defaultAgentPort = 18800

// portWaitTimeout bounds the wait for a restarted pod's new mapping and SSH.
const portWaitTimeout = 5 * time.Minute

// podPort is one entry of a pod's runtime.ports.
type podPort struct {
	IP          string          `json:"ip"`
	Private     json.RawMessage `json:"private"`
	Public      json.RawMessage `json:"public"`
	PrivatePort json.RawMessage `json:"privatePort"`
	PublicPort  json.RawMessage `json:"publicPort"`
	Type        string          `json:"type"`
}

// tcpMappings are the pod's TCP ports reachable from outside: container port
// → public port, and the public address they are on. HTTP ports go through
// RunPod's proxy, which cannot carry the agent's connection.
func (p *pod) tcpMappings() (string, map[int]int) {
	mapped := map[int]int{}
	ip := ""
	if len(p.Runtime) == 0 {
		return ip, mapped
	}
	var runtime struct {
		Ports []podPort `json:"ports"`
	}
	if err := json.Unmarshal(p.Runtime, &runtime); err != nil {
		return ip, mapped
	}
	for _, port := range runtime.Ports {
		if !strings.EqualFold(port.Type, "tcp") || port.IP == "" {
			continue
		}
		private, _ := jsonInt(firstRaw(port.Private, port.PrivatePort))
		public, _ := jsonInt(firstRaw(port.Public, port.PublicPort))
		if private < 1 || private > 65535 || public < 1 || public > 65535 {
			continue
		}
		mapped[int(private)] = int(public)
		ip = port.IP
	}
	return ip, mapped
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, v := range values {
		if len(v) > 0 && string(v) != "null" {
			return v
		}
	}
	return nil
}

// exposedPorts is the pod's ports setting ("22/tcp", "8888/http", ...),
// which an edit replaces whole.
func (p *pod) exposedPorts() []string {
	var ports []string
	if err := json.Unmarshal(p.Ports, &ports); err == nil {
		return ports
	}
	var joined string
	if err := json.Unmarshal(p.Ports, &joined); err == nil && strings.TrimSpace(joined) != "" {
		return strings.Split(joined, ",")
	}
	return nil
}

// newAgentPort is the container port to expose for the agent: the one asked
// for, else defaultAgentPort, moved past any port the pod already exposes in
// any protocol.
func newAgentPort(exposed []string, preferred int) int {
	taken := map[int]bool{}
	for _, entry := range exposed {
		port, _, _ := strings.Cut(strings.TrimSpace(entry), "/")
		if n, err := strconv.Atoi(port); err == nil {
			taken[n] = true
		}
	}
	port := preferred
	if port <= 0 {
		port = defaultAgentPort
	}
	for taken[port] && port < 65535 {
		port++
	}
	return port
}

// ServerDialAddressFor implements provider.ServerDialPortMapper.
func (d *Driver) ServerDialAddressFor(ctx context.Context, creds provider.Credentials, externalID, listen string) (string, error) {
	p, err := d.getPod(ctx, creds, externalID)
	if err != nil {
		return "", err
	}
	port := provider.PortOf(listen)
	ip, mapped := p.tcpMappings()
	public, ok := mapped[port]
	if !ok {
		return "", fmt.Errorf("runpod: pod %s exposes no TCP mapping for container port %d", externalID, port)
	}
	return net.JoinHostPort(ip, strconv.Itoa(public)), nil
}

// exposePort adds port/tcp to the pod's exposed ports, which restarts it, and
// waits until the restarted pod maps that port and its SSH answers again.
func (d *Driver) exposePort(ctx context.Context, creds provider.Credentials, p pod, port int, ready func(provider.Instance) error) (pod, error) {
	changes := map[string]any{"ports": append(p.exposedPorts(), strconv.Itoa(port)+"/tcp")}
	return d.editPod(ctx, creds, p, changes, fmt.Sprintf("port %d mapped and SSH answering", port), func(next pod) error {
		if _, mapped := next.tcpMappings(); mapped[port] == 0 {
			return fmt.Errorf("port %d is not mapped yet", port)
		}
		return ready(next.toInstance())
	})
}

func (d *Driver) portWait() time.Duration {
	if d.portWaitTimeout > 0 {
		return d.portWaitTimeout
	}
	return portWaitTimeout
}

func (d *Driver) portPoll() time.Duration {
	if d.portPollInterval > 0 {
		return d.portPollInterval
	}
	return 5 * time.Second
}

// listeningPorts reads which ports something on the pod listens on.
func (d *Driver) listeningPorts(ctx context.Context, inst provider.Instance, auth provision.SSHAuth) (map[int]bool, error) {
	if d.listening != nil {
		return d.listening(ctx, inst, auth)
	}
	client, _, err := provision.DialSSH(ctx, firstNonEmpty(inst.SSHUser, "root"), inst.PublicIP, inst.SSHPort, auth, 15*time.Second, nil)
	if err != nil {
		return nil, provider.RetryableSSHConnectionError("runpod: ssh "+net.JoinHostPort(inst.PublicIP, strconv.Itoa(inst.SSHPort)), err)
	}
	defer client.Close()
	return provision.ListeningPorts(ctx, client)
}

// serverDialPort chooses where a server-dialed agent on the pod listens and
// where the server reaches it, exposing a port (a restart) when the pod has
// no spare one and the operator agreed. It returns the pod as the install
// should reach it: the edit renumbers SSH too.
func (d *Driver) serverDialPort(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (listen, address string, inst provider.Instance, check provision.CheckResult, err error) {
	p, err := d.getPod(ctx, creds, req.Instance.ExternalID)
	if err != nil {
		return "", "", provider.Instance{}, check, err
	}
	inst = p.toInstance()
	if inst.SSHPort == 0 {
		return "", "", provider.Instance{}, check, fmt.Errorf("runpod: no ssh access for pod %q; install manually", p.ID)
	}
	auth := req.SSHAuth()
	listening, err := d.listeningPorts(ctx, inst, auth)
	if err != nil {
		return "", "", provider.Instance{}, check, err
	}
	ip, mapped := p.tcpMappings()
	preferred := provider.PortOf(req.ServerDialListen)
	if port, ok := provider.ChooseMappedPort(mapped, listening, preferred); ok {
		address = net.JoinHostPort(ip, strconv.Itoa(mapped[port]))
		check = provider.ServerDialPortCheck(port, address, false, false, fmt.Sprintf("container port %d, exposed as %s", port, address))
		return provider.ListenOnPort(port), address, inst, check, nil
	}
	if !req.AllowInstanceRestart {
		return "", "", provider.Instance{}, check, &provider.InstanceRestartError{Reason: restartReason(needPort), Need: needPort}
	}
	port := newAgentPort(p.exposedPorts(), preferred)
	p, err = d.exposePort(ctx, creds, p, port, func(next provider.Instance) error {
		_, err := d.listeningPorts(ctx, next, auth)
		return err
	})
	if err != nil {
		return "", "", provider.Instance{}, check, err
	}
	ip, mapped = p.tcpMappings()
	address = net.JoinHostPort(ip, strconv.Itoa(mapped[port]))
	check = provider.ServerDialPortCheck(port, address, true, true, fmt.Sprintf("exposed container port %d as %s; the pod restarted", port, address))
	return provider.ListenOnPort(port), address, p.toInstance(), check, nil
}
