package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

type doctorNodes struct {
	online     bool
	raw        json.RawMessage
	err        error
	disconnect bool
	calls      int
}

func (n *doctorNodes) Online(string) bool { return n.online }
func (n *doctorNodes) CallNode(context.Context, string, string, any) (json.RawMessage, error) {
	n.calls++
	if n.disconnect {
		n.online = false
	}
	return n.raw, n.err
}

func checkByStage(t *testing.T, report DoctorReport, stage string) DoctorCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Stage == stage {
			return check
		}
	}
	t.Fatalf("missing %s", stage)
	return DoctorCheck{}
}

func TestDoctorAccessOnlinePreservesLegacyAndStructuredReport(t *testing.T) {
	for _, raw := range []string{`{"report":"legacy"}`, `{"report":"ok","host":{"hostname":"node"}}`} {
		nodes := &doctorNodes{online: true, raw: json.RawMessage(raw)}
		d := DoctorService{Nodes: nodes, Settings: func(string) (ConnectionNode, error) { return ConnectionNode{}, nil }, Connection: ConnectionDiagnoser{Inspect: func(context.Context, string) provision.AccessInspection {
			t.Fatal("online node ran access diagnostics")
			return provision.AccessInspection{}
		}}}
		got, err := d.Diagnose(context.Background(), "node", true)
		if err != nil || len(got.Checks) != 3 || nodes.calls != 1 || got.Report == "" {
			t.Fatalf("%+v %v", got, err)
		}
	}
}

func TestDoctorAccessSSHDirectionMatrixReturnsPartialReport(t *testing.T) {
	for _, ssh := range []bool{false, true} {
		for _, mode := range []string{"agent_dial", "server_dial"} {
			t.Run(mode+map[bool]string{true: " with SSH", false: " without SSH"}[ssh], func(t *testing.T) {
				node := ConnectionNode{UUID: "node", Mode: mode, ServerURL: "http://server:8800", Address: "127.0.0.1:18800"}
				nodes := &doctorNodes{}
				probes := 0
				d := DoctorService{Nodes: nodes, Settings: func(string) (ConnectionNode, error) { return node, nil }, Connection: ConnectionDiagnoser{
					Inspect: func(context.Context, string) provision.AccessInspection {
						if !ssh {
							return provision.NewAccessInspection(messages.DoctorCredentialsUnavailable)
						}
						return provision.AccessInspection{SSH: proto.CheckPass, Installation: proto.CheckPass, Process: proto.CheckPass, ObservedAt: time.Now(), Connection: provision.ConnectionCheck{Outcome: "http_ok", Target: node.ServerURL + "/healthz", ObservedAt: time.Now()}}.WithConnectionResult()
					},
					Dial: func(context.Context, string, string) (net.Conn, error) {
						probes++
						return nil, context.DeadlineExceeded
					},
				}}
				got, err := d.Diagnose(context.Background(), "node", true)
				if err != nil || nodes.calls != 0 || len(got.Checks) != 8 {
					t.Fatalf("%+v %v", got, err)
				}
				var stages []string
				for _, check := range got.Checks {
					stages = append(stages, check.Stage)
				}
				if strings.Join(stages, ",") != "ssh,installation,process,connection,authentication,host,runtimes,accelerators" {
					t.Fatalf("unexpected checks: %v", stages)
				}
				process := checkByStage(t, got, "process")
				if ssh && process.Status != proto.CheckPass || !ssh && process.Status != proto.CheckSkip {
					t.Fatalf("%+v", process)
				}
				connection := checkByStage(t, got, "connection")
				if mode == "server_dial" {
					if probes != 1 || connection.Status != proto.CheckFail || connection.Code != "TIMEOUT" || connection.Vantage != "control_plane" {
						t.Fatalf("%+v probes=%d", connection, probes)
					}
				} else if probes != 0 || (ssh && connection.Status != proto.CheckPass) || (!ssh && connection.Status != proto.CheckSkip) {
					t.Fatalf("wrong direction %+v", connection)
				}
				if checkByStage(t, got, "authentication").Status != proto.CheckSkip {
					t.Fatal("network failure became authentication failure")
				}
			})
		}
	}
}

func TestDoctorAccessTCPReachabilityNeverProvesAuthentication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	d := ConnectionDiagnoser{}
	report := d.Diagnose(context.Background(), ConnectionNode{Mode: "server_dial", Address: listener.Addr().String()}, true)
	if checkByStage(t, report, "connection").Status != proto.CheckPass || checkByStage(t, report, "authentication").Status != proto.CheckSkip {
		t.Fatalf("%+v", report)
	}
}

func TestDoctorAccessRefusalAndSSHFailureAreIndependent(t *testing.T) {
	d := ConnectionDiagnoser{Inspect: func(context.Context, string) provision.AccessInspection {
		return provision.AccessInspection{SSH: proto.CheckFail, Code: messages.DoctorSSHUnavailable}
	}, Dial: func(context.Context, string, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }}
	report := d.Diagnose(context.Background(), ConnectionNode{Mode: "server_dial", Address: "127.0.0.1:18800"}, true)
	if checkByStage(t, report, "connection").Code != "REFUSED" || checkByStage(t, report, "installation").Status != proto.CheckSkip {
		t.Fatalf("%+v", report)
	}
}

func TestDoctorAccessKeepsPartialEvidenceAfterInspectionTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, mode := range []string{"agent_dial", "server_dial"} {
		t.Run(mode, func(t *testing.T) {
			d := ConnectionDiagnoser{Inspect: func(context.Context, string) provision.AccessInspection {
				return provision.AccessInspection{SSH: proto.CheckPass, Installation: proto.CheckPass, Process: proto.CheckSkip, Code: messages.DoctorTimeout, ObservedAt: time.Now()}
			}}
			report := d.Diagnose(context.Background(), ConnectionNode{Mode: mode, ServerURL: "http://server", Address: listener.Addr().String()}, true)
			if installed := checkByStage(t, report, "installation"); installed.Status != proto.CheckPass || installed.ObservedAt == nil {
				t.Fatalf("completed installation evidence lost: %+v", installed)
			}
			if process := checkByStage(t, report, "process"); process.Status != proto.CheckSkip || process.Code != "TIMEOUT" {
				t.Fatalf("unfinished process inspection acquired a verdict: %+v", process)
			}
			connection := checkByStage(t, report, "connection")
			if mode == "agent_dial" {
				if connection.Status != proto.CheckSkip || connection.Code != "TIMEOUT" || connection.ObservedAt != nil {
					t.Fatalf("unexecuted HTTP probe acquired a verdict: %+v", connection)
				}
			} else if connection.Status != proto.CheckPass || connection.Code != "TCP_CONNECTED" {
				t.Fatalf("SSH timeout blocked independent TCP probe: %+v", connection)
			}
		})
	}
}

func TestDoctorAccessPermissionAndCancellation(t *testing.T) {
	d := ConnectionDiagnoser{Inspect: func(context.Context, string) provision.AccessInspection {
		t.Fatal("unauthorized inspection")
		return provision.AccessInspection{}
	}, Dial: func(context.Context, string, string) (net.Conn, error) { t.Fatal("unauthorized dial"); return nil, nil }}
	report := d.Diagnose(context.Background(), ConnectionNode{Mode: "server_dial", Address: "127.0.0.1:18800"}, false)
	for _, c := range report.Checks {
		if c.Status != proto.CheckSkip || c.Code != "ADMINISTRATOR_REQUIRED" {
			t.Fatalf("%+v", c)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report = ConnectionDiagnoser{}.Diagnose(ctx, ConnectionNode{Mode: "server_dial", Address: "127.0.0.1:18800"}, true)
	if c := checkByStage(t, report, "connection"); c.Status != proto.CheckSkip || c.Code != "CANCELLED" {
		t.Fatalf("%+v", c)
	}
}

func TestDoctorAccessDisconnectDuringRPCAndMalformedResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nodes   doctorNodes
		access  bool
		wantErr error
	}{
		{"disconnected", doctorNodes{online: true, disconnect: true, err: errors.New("disconnected")}, true, nil},
		{"still online RPC error", doctorNodes{online: true, err: errors.New("remote failure")}, false, errors.New("remote failure")},
		{"bad response", doctorNodes{online: true, raw: json.RawMessage(`{"host":42}`)}, false, ErrDoctorResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DoctorService{Nodes: &tc.nodes, Settings: func(string) (ConnectionNode, error) { return ConnectionNode{Mode: "agent_dial"}, nil }}
			r, err := d.Diagnose(context.Background(), "node", true)
			if (len(r.Checks) == 8) != tc.access || (err != nil) != (tc.wantErr != nil) {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestDoctorAccessNodeHTTPChecksRemainSeparateFromAuthentication(t *testing.T) {
	for _, tc := range []struct{ outcome, status, reason string }{
		{"http_ok", proto.CheckPass, "HTTP_CONNECTED"},
		{"http_error", proto.CheckFail, "HTTP_ERROR"},
		{"timeout", proto.CheckFail, "TIMEOUT"},
		{"dns", proto.CheckFail, "DNS"},
		{"tls", proto.CheckFail, "TLS"},
		{"tool_missing", proto.CheckSkip, "TOOL_MISSING"},
		{"identity_unverified", proto.CheckSkip, "IDENTITY_UNVERIFIED"},
		{"secret=private", proto.CheckSkip, "INSPECTION_UNAVAILABLE"},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			at := time.Now().UTC()
			d := ConnectionDiagnoser{Inspect: func(context.Context, string) provision.AccessInspection {
				return provision.AccessInspection{SSH: proto.CheckPass, Connection: provision.ConnectionCheck{Outcome: tc.outcome, Target: "http://server/healthz", ObservedAt: at}}.WithConnectionResult()
			}, Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("agent_dial probed from Server")
				return nil, nil
			}}
			r := d.Diagnose(context.Background(), ConnectionNode{Mode: "agent_dial", ServerURL: "http://server/"}, true)
			c := checkByStage(t, r, "connection")
			if c.Status != tc.status || c.Code != tc.reason || c.Source != "http_probe" || c.Vantage != "node" || c.ObservedAt == nil || !c.ObservedAt.Equal(at) {
				t.Fatalf("%+v", c)
			}
			if auth := checkByStage(t, r, "authentication"); auth.Status != proto.CheckSkip || auth.ObservedAt != nil {
				t.Fatalf("HTTP check invented authentication: %+v", auth)
			}
		})
	}
}

func TestDoctorAccessRunsNewChecksForEachRequest(t *testing.T) {
	calls := 0
	d := ConnectionDiagnoser{Inspect: func(context.Context, string) provision.AccessInspection {
		calls++
		outcome := "timeout"
		if calls == 2 {
			outcome = "http_ok"
		}
		return provision.AccessInspection{SSH: proto.CheckPass, Connection: provision.ConnectionCheck{Target: "http://server/healthz", Outcome: outcome, ObservedAt: time.Now()}}.WithConnectionResult()
	}}
	node := ConnectionNode{Mode: "agent_dial", ServerURL: "http://server"}
	first := d.Diagnose(context.Background(), node, true)
	second := d.Diagnose(context.Background(), node, true)
	if calls != 2 || checkByStage(t, first, "connection").Status != proto.CheckFail || checkByStage(t, second, "connection").Status != proto.CheckPass {
		t.Fatalf("reused previous result: first=%+v second=%+v calls=%d", first, second, calls)
	}
}

func TestDoctorAccessRejectsUnrelatedOrUnsafeTarget(t *testing.T) {
	d := ConnectionDiagnoser{Inspect: func(context.Context, string) provision.AccessInspection {
		return provision.AccessInspection{SSH: proto.CheckPass, Connection: provision.ConnectionCheck{Target: "http://other/healthz", Outcome: "http_ok"}}.WithConnectionResult()
	}}
	for _, target := range []string{"http://server", "http://user:secret@server", "http://server?secret=private", "http://server/private", "http://server/%70rivate"} {
		r := d.Diagnose(context.Background(), ConnectionNode{Mode: "agent_dial", ServerURL: target, Token: "private"}, true)
		c := checkByStage(t, r, "connection")
		if c.Status != proto.CheckSkip || strings.Contains(c.Target, "secret") || strings.Contains(c.Target, "private") {
			t.Fatalf("unrelated/unsafe target used: %+v", c)
		}
	}
}
