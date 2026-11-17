package compshare

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// procNetTCP is /proc/net/tcp with LISTEN (0A) rows for the given ports, as
// the container's kernel prints it.
func procNetTCP(ports ...int) string {
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i, port := range ports {
		fmt.Fprintf(&b, "   %d: 00000000:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 %d 1 0000000000000000 100 0 0 10 0\n", i, port, 1000+i)
	}
	return b.String()
}

// procSSHServer accepts root/pw and answers every command with procNetTCP
// of listening.
func procSSHServer(t *testing.T, listening ...int) (string, int) {
	t.Helper()
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(hostKey)
	cfg := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if meta.User() == "root" && string(password) == "pw" {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	out := procNetTCP(listening...)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, requests, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer ch.Close()
						for req := range requests {
							if req.Type != "exec" {
								_ = req.Reply(false, nil)
								continue
							}
							_ = req.Reply(true, nil)
							_, _ = ch.Write([]byte(out))
							_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							return
						}
					}()
				}
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

// fakePortsAPI answers DescribeCompShareInstance with a container instance
// shaped like cpod-1wtznikn04ks (2026-10-23) and applies
// UpdateCompShareInstancePorts the way the API does: the list replaces the
// open TCP ports and each new one is forwarded from the shared host.
type fakePortsAPI struct {
	mu       sync.Mutex
	kind     string
	http     []int
	tcp      []int
	forwards []tcpForward
	updates  []map[string]string
	next     int
}

func (f *fakePortsAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		params := map[string]string{}
		for key := range r.Form {
			if key != "Signature" {
				params[key] = r.Form.Get(key)
			}
		}
		if r.Form.Get("Signature") != sign(params, "priv") {
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 171, "Message": "signature error"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch params["Action"] {
		case listAction:
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "TotalCount": 1, "UHostSet": []map[string]any{{
				"UHostId": "cpod-1wtznikn04ks", "Region": "cn-sh2", "Zone": "cn-sh2-03", "State": "Running",
				"InstanceType": f.kind, "MachineType": "G",
				"SshLoginCommand": "ssh -p 23148 root@cpod-1wtznikn04ks-s3.podtcp.compshare.cn",
				"Ports":           map[string]any{"HttpPorts": f.http, "TcpPorts": f.tcp, "UdpPorts": []int{}},
				"TcpForwards":     f.forwards,
			}}})
		case "UpdateCompShareInstancePorts":
			f.updates = append(f.updates, params)
			if params["Region"] != "cn-sh2" || params["Zone"] != "cn-sh2-03" || params["UHostId"] != "cpod-1wtznikn04ks" {
				_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 230, "Message": "Params [Zone] not available"})
				return
			}
			var tcp []int
			for i := 0; params["TcpPorts."+strconv.Itoa(i)] != ""; i++ {
				port, _ := strconv.Atoi(params["TcpPorts."+strconv.Itoa(i)])
				tcp = append(tcp, port)
			}
			var forwards []tcpForward
			for _, port := range tcp {
				external := 0
				for _, old := range f.forwards {
					if old.InternalPort == port {
						external = old.ExternalPort
					}
				}
				if external == 0 {
					external = 30006 + f.next
					f.next++
				}
				forwards = append(forwards, tcpForward{ExternalHost: "cpod-1wtznikn04ks-s3.podtcp.compshare.cn", ExternalPort: external, InternalPort: port})
			}
			f.tcp, f.forwards = tcp, forwards
			_ = json.NewEncoder(w).Encode(map[string]any{"Action": "UpdateCompShareInstancePortsResponse", "RetCode": 0, "UHostId": params["UHostId"], "TcpForwards": forwards})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 160, "Message": "Action [" + params["Action"] + "] not found"})
		}
	}
}

func newPortsAPI(t *testing.T) (*Driver, *fakePortsAPI) {
	t.Helper()
	api := &fakePortsAPI{kind: "Container", http: []int{8888, 8889}, tcp: []int{23},
		forwards: []tcpForward{{ExternalHost: "cpod-1wtznikn04ks-s3.podtcp.compshare.cn", ExternalPort: 23148, InternalPort: 23}}}
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	d := New()
	d.setBaseURL(srv.URL)
	return d, api
}

var portsCreds = provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}

func containerInstallRequest(host string, port int, listen string) provider.InstallRequest {
	return provider.InstallRequest{
		Instance: provider.Instance{ExternalID: "cpod-1wtznikn04ks", PublicIP: host, SSHPort: port, SSHUser: "root", Kind: "container"},
		Token:    "tok", ConnectMode: "server_dial", ServerDialListen: listen, InstancePassword: "pw",
		InstanceKind: "container",
	}
}

// A container opens only SSH's port: the install opens the agent's port
// with every port already open kept, and the server dials it where the API
// forwards it from.
func TestServerDialPortOpensAPortWhenNoneIsFree(t *testing.T) {
	d, api := newPortsAPI(t)
	host, port := procSSHServer(t, 23)
	listen, address, check, err := d.serverDialPort(context.Background(), portsCreds, containerInstallRequest(host, port, ":3389"))
	if err != nil {
		t.Fatal(err)
	}
	if listen != "0.0.0.0:3389" || address != "cpod-1wtznikn04ks-s3.podtcp.compshare.cn:30006" {
		t.Fatalf("listen=%q address=%q", listen, address)
	}
	if check.Status != "pass" || !strings.Contains(checkText(check), "opened container port 3389") {
		t.Fatalf("check = %+v", check)
	}
	if len(api.updates) != 1 {
		t.Fatalf("updates = %v", api.updates)
	}
	got := api.updates[0]
	want := map[string]string{"HttpPorts.0": "8888", "HttpPorts.1": "8889", "TcpPorts.0": "23", "TcpPorts.1": "3389"}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("update %s = %q, want %q (all: %v)", key, got[key], value, got)
		}
	}
	if _, extra := got["TcpPorts.2"]; extra {
		t.Fatalf("update sent extra ports: %v", got)
	}
	// Recovery later reads the forward back from the instance.
	again, err := d.ServerDialAddressFor(context.Background(), portsCreds, "cpod-1wtznikn04ks", listen)
	if err != nil || again != address {
		t.Fatalf("ServerDialAddressFor = %q, %v", again, err)
	}
}

// A port the operator opened in the console and nothing holds is used as
// is; SSH's port is never given away.
func TestServerDialPortUsesAnOpenFreePort(t *testing.T) {
	d, api := newPortsAPI(t)
	api.tcp = append(api.tcp, 7000)
	api.forwards = append(api.forwards, tcpForward{ExternalHost: "cpod-1wtznikn04ks-s3.podtcp.compshare.cn", ExternalPort: 31000, InternalPort: 7000})
	host, port := procSSHServer(t, 23)
	listen, address, check, err := d.serverDialPort(context.Background(), portsCreds, containerInstallRequest(host, port, ":3389"))
	if err != nil {
		t.Fatal(err)
	}
	if listen != "0.0.0.0:7000" || address != "cpod-1wtznikn04ks-s3.podtcp.compshare.cn:31000" || !strings.Contains(checkText(check), "already open") {
		t.Fatalf("listen=%q address=%q check=%+v", listen, address, check)
	}
	if len(api.updates) != 0 {
		t.Fatalf("opened a port although one was free: %v", api.updates)
	}
}

// The preferred port is held by something in the container: the next free
// one is opened instead.
func TestServerDialPortSkipsAHeldPort(t *testing.T) {
	d, api := newPortsAPI(t)
	host, port := procSSHServer(t, 23, 3389)
	listen, _, _, err := d.serverDialPort(context.Background(), portsCreds, containerInstallRequest(host, port, ""))
	if err != nil {
		t.Fatal(err)
	}
	if listen != "0.0.0.0:3390" || len(api.updates) != 1 || api.updates[0]["TcpPorts.1"] != "3390" {
		t.Fatalf("listen=%q updates=%v", listen, api.updates)
	}
}

// CompShare opens at most ten TCP ports: with all taken the install says
// what to do instead of sending an update the API refuses.
func TestServerDialPortReportsTheTCPPortLimit(t *testing.T) {
	d, api := newPortsAPI(t)
	held := []int{23}
	for p := 7000; p < 7009; p++ {
		api.tcp = append(api.tcp, p)
		api.forwards = append(api.forwards, tcpForward{ExternalHost: "h", ExternalPort: p + 20000, InternalPort: p})
		held = append(held, p)
	}
	host, port := procSSHServer(t, held...)
	_, _, _, err := d.serverDialPort(context.Background(), portsCreds, containerInstallRequest(host, port, ":3389"))
	if err == nil || !strings.Contains(err.Error(), "the most CompShare allows") {
		t.Fatalf("err = %v", err)
	}
	if len(api.updates) != 0 {
		t.Fatalf("updates = %v", api.updates)
	}
}

// An SSH failure is a connection error the service may retry another way.
func TestServerDialPortSSHFailureIsRetryable(t *testing.T) {
	d, _ := newPortsAPI(t)
	req := containerInstallRequest("127.0.0.1", 1, ":3389")
	_, _, _, err := d.serverDialPort(context.Background(), portsCreds, req)
	if !provider.IsRetryableSSHConnectionError(err) {
		t.Fatalf("err = %v, want retryable SSH connection error", err)
	}
}

// Only a container's ports are forwarded: a VM keeps the address the
// install recorded.
func TestServerDialPortMappingIsForContainersOnly(t *testing.T) {
	d, api := newPortsAPI(t)
	if !provider.MapsServerDialPorts(d, provider.Instance{Kind: "container"}) || provider.MapsServerDialPorts(d, provider.Instance{Kind: "vm"}) {
		t.Fatal("MapsServerDialPorts should hold for containers only")
	}
	api.kind = "UHost"
	if _, err := d.ServerDialAddressFor(context.Background(), portsCreds, "cpod-1wtznikn04ks", "0.0.0.0:3389"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("VM ServerDialAddressFor err = %v, want ErrUnsupported", err)
	}
	api.kind = "Container"
	if _, err := d.ServerDialAddressFor(context.Background(), portsCreds, "cpod-1wtznikn04ks", "0.0.0.0:3389"); err == nil || !strings.Contains(err.Error(), "no longer opens container port 3389") {
		t.Fatalf("unforwarded port: %v", err)
	}
}
