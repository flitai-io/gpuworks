package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// RunPod lets the server change a running pod only by editing its
// configuration, which restarts the container (clearing the container disk)
// and renumbers every mapped port, SSH's included (observed 2026-10-19). Two
// things the server may need that way, each only with the operator's
// consent (InstallRequest.AllowInstanceRestart): a TCP port for an agent the
// server dials, and the platform SSH key on a pod started before the key was
// in the account (RunPod writes the account's keys, through the pod's
// PUBLIC_KEY variable, only when a pod starts). One edit does both when both
// are needed.

// What a restart is asked for (provider.InstanceRestartError.Need).
const (
	needPort      = "port"
	needSSHKey    = "ssh_key"
	needSSHKeyAnd = "ssh_key_port"
)

func restartReason(need string) string {
	switch need {
	case needSSHKey:
		return "RunPod adds an SSH key to a running pod only by changing the pod's configuration, which restarts its container and clears everything not on a volume"
	case needSSHKeyAnd:
		return "RunPod adds an SSH key and opens a port only by changing the pod's configuration, which restarts its container and clears everything not on a volume"
	default:
		return "RunPod opens a port only by changing the pod's configuration, which restarts its container and clears everything not on a volume"
	}
}

// podEnv is the pod's environment, which an edit replaces whole.
func (p *pod) podEnv() map[string]string {
	env := map[string]string{}
	_ = json.Unmarshal(p.Env, &env)
	return env
}

// hasPublicKey reports whether the pod's PUBLIC_KEY carries key (compared
// by type and key material, not comment).
func (p *pod) hasPublicKey(key string) bool {
	want := strings.Fields(key)
	if len(want) < 2 {
		return false
	}
	for _, line := range strings.Split(p.podEnv()["PUBLIC_KEY"], "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == want[0] && f[1] == want[1] {
			return true
		}
	}
	return false
}

// withPublicKey is the pod's environment with key appended to PUBLIC_KEY.
func (p *pod) withPublicKey(key string) map[string]string {
	env := p.podEnv()
	keys := strings.TrimRight(env["PUBLIC_KEY"], "\n")
	if keys != "" {
		keys += "\n"
	}
	env["PUBLIC_KEY"] = keys + strings.TrimSpace(key)
	return env
}

// login tries to log in to the pod with auth.
func (d *Driver) login(ctx context.Context, inst provider.Instance, auth provision.SSHAuth) error {
	if d.loginHook != nil {
		return d.loginHook(ctx, inst, auth)
	}
	client, _, err := provision.DialSSH(ctx, firstNonEmpty(inst.SSHUser, "root"), inst.PublicIP, inst.SSHPort, auth, 15*time.Second, nil)
	if err != nil {
		return err
	}
	return client.Close()
}

// editPod applies changes (a PodUpdateInput: ports, env) to the pod, which
// restarts it, and waits until ready accepts the restarted pod. what names
// the wait in errors.
func (d *Driver) editPod(ctx context.Context, creds provider.Credentials, p pod, changes map[string]any, what string, ready func(pod) error) (pod, error) {
	resp, err := d.doJSON(ctx, creds, http.MethodPatch, "/pods/"+url.PathEscape(p.ID), changes)
	if err != nil {
		return pod{}, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return pod{}, fmt.Errorf("runpod: edit pod %s: status %d", p.ID, resp.StatusCode)
	}
	deadline := time.Now().Add(d.portWait())
	var last error
	for {
		next, err := d.getPod(ctx, creds, p.ID)
		if err == nil {
			if next.toInstance().SSHPort == 0 {
				err = errors.New("no SSH endpoint yet")
			} else {
				err = ready(next)
			}
			if err == nil {
				return next, nil
			}
		}
		last = err
		if time.Now().After(deadline) {
			return pod{}, fmt.Errorf("runpod: pod %s did not come back with %s within %s: %v", p.ID, what, d.portWait(), last)
		}
		select {
		case <-ctx.Done():
			return pod{}, ctx.Err()
		case <-time.After(d.portPoll()):
		}
	}
}

// ensurePlatformKey makes the pod accept the server's logins. A pod that
// lists the platform key, or that already accepts the credentials at hand,
// is left alone; otherwise, with the operator's consent, the key is added to
// the pod's PUBLIC_KEY — and, for an agent the server dials on a pod with no
// TCP port besides SSH's, a port too, so one restart does both. It returns
// the instance to log in to (a restart renumbers SSH) and a check for the
// install report ("" Name when nothing was done).
func (d *Driver) ensurePlatformKey(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.Instance, provider.InstallCheck, error) {
	inst := req.Instance
	if req.PlatformKey == nil || strings.TrimSpace(req.PlatformKey.PublicKey) == "" {
		return inst, provider.InstallCheck{}, nil
	}
	p, err := d.getPod(ctx, creds, req.Instance.ExternalID)
	if err != nil {
		return inst, provider.InstallCheck{}, nil //nolint:nilerr // Key refresh is optional; installation reports endpoint reachability separately.
	}
	if fresh := p.toInstance(); fresh.SSHPort != 0 {
		inst = fresh
	}
	key := req.PlatformKey.PublicKey
	if p.hasPublicKey(key) {
		return inst, provider.InstallCheck{}, nil
	}
	auth := req.SSHAuth()
	var authErr *provision.SSHAuthError
	if err := d.login(ctx, inst, auth); err == nil || !errors.As(err, &authErr) {
		// It logs in already, or cannot be reached at all: the install says
		// which, and an edit would not help.
		return inst, provider.InstallCheck{}, nil
	}
	_, mapped := p.tcpMappings()
	addPort := req.ConnectMode == "server_dial" && strings.TrimSpace(req.ServerDialAddress) == "" && len(filterSSH(mapped)) == 0
	need := needSSHKey
	if addPort {
		need = needSSHKeyAnd
	}
	if !req.AllowInstanceRestart {
		return inst, provider.InstallCheck{}, &provider.InstanceRestartError{Reason: restartReason(need), Need: need}
	}
	changes := map[string]any{"env": p.withPublicKey(key)}
	port := 0
	if addPort {
		port = newAgentPort(p.exposedPorts(), provider.PortOf(req.ServerDialListen))
		changes["ports"] = append(p.exposedPorts(), strconv.Itoa(port)+"/tcp")
	}
	keyAuth := provision.SSHAuth{PlatformKey: auth.PlatformKey}
	p, err = d.editPod(ctx, creds, p, changes, "the platform key accepted", func(next pod) error {
		if port != 0 {
			if _, mapped := next.tcpMappings(); mapped[port] == 0 {
				return fmt.Errorf("port %d is not mapped yet", port)
			}
		}
		return d.login(ctx, next.toInstance(), keyAuth)
	})
	if err != nil {
		return inst, provider.InstallCheck{}, err
	}
	inst = p.toInstance()
	detail := "added the platform key to the pod's PUBLIC_KEY; the pod restarted"
	if port != 0 {
		detail = fmt.Sprintf("added the platform key to the pod's PUBLIC_KEY and exposed container port %d; the pod restarted", port)
	}
	return inst, provider.InstallCheck{Name: "Platform key (pod)", Status: "ok", Detail: detail + " (SSH now " + net.JoinHostPort(inst.PublicIP, strconv.Itoa(inst.SSHPort)) + ")"}, nil
}

// PlatformKeyAtInstall implements provider.InstallKeyAdder: the install adds
// the key to a pod whose PUBLIC_KEY lacks it, once the operator agrees to
// the restart.
func (d *Driver) PlatformKeyAtInstall(ctx context.Context, creds provider.Credentials, inst provider.Instance, publicKey string) (provider.InstallCheck, bool) {
	p, err := d.getPod(ctx, creds, inst.ExternalID)
	if err != nil || p.hasPublicKey(publicKey) {
		return provider.InstallCheck{}, false
	}
	return provider.InstallCheck{Status: "warn", Detail: "the pod started before the platform key was in the RunPod account; the install adds it to the pod, which restarts the pod and clears its container disk, after asking (the remaining checks run once SSH logs in)"}, true
}

// filterSSH is mapped without SSH's port.
func filterSSH(mapped map[int]int) map[int]int {
	out := map[int]int{}
	for port, public := range mapped {
		if port != 22 {
			out[port] = public
		}
	}
	return out
}
