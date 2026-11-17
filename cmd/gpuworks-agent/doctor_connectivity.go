package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
)

var doctorEnvironment = agentapp.DoctorForState

// enableHostCLIsForKubeDoctor is gpu.RunCLIsOnHost; a test overrides it to
// observe the decision without flipping the real, one-way switch gpu.go's
// cliOnHost is (it is shared by the whole test binary, the same seaming
// reason doctor_nvidia.go's pythonCommand and cliOnHostForDoctor exist).
var enableHostCLIsForKubeDoctor = gpu.RunCLIsOnHost

func doctor(ctx context.Context, log *slog.Logger, args []string, out, stderr io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // flag errors can contain caller-supplied credentials.
	launch := newLaunchFlags(fs)
	connectivity := fs.Bool("connectivity", false, "check the node-to-server connection only (no GPU checks)")
	asJSON := fs.Bool("json", false, "write a connection DoctorReport as JSON; requires --connectivity")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "Usage of doctor:")
			fs.SetOutput(stderr)
			fs.PrintDefaults()
			return err
		}
		return &exitCodeError{code: 2, err: errors.New("invalid doctor arguments")}
	}
	if fs.NArg() != 0 {
		return &exitCodeError{code: 2, err: errors.New("doctor: unexpected argument")}
	}
	if !*connectivity {
		invalid := *asJSON
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "state" && f.Name != "connectivity" {
				invalid = true
			}
		})
		if invalid {
			return &exitCodeError{code: 2, err: errors.New("connection options require --connectivity")}
		}
		state := launch.state
		if state == "" {
			state = defaultStateDir()
		}
		// `doctor` runs as its own process (typically `kubectl exec` into
		// the running agent pod), so it never goes through `run`'s
		// prepareKubeRun, which is what otherwise turns this switch on. A
		// Kubernetes DaemonSet pod's env still carries the node name the
		// manifest sets either way, so that alone says every vendor CLI and
		// device file must be looked up on the host, not in this container.
		if strings.TrimSpace(os.Getenv("GPUWORKS_KUBE_NODE_NAME")) != "" {
			enableHostCLIsForKubeDoctor()
		}
		doctorEnvironment(ctx, out, state)
		return nil
	}
	conf, state, fromFile, err := launch.resolve(os.Getenv)
	var report proto.DoctorReport
	if err != nil {
		// Never echo the raw parse/validation error or its file contents.
		report = proto.DoctorReport{Facts: []proto.DoctorFact{{Code: "connection.source", Value: "cli"}, {Code: "connection.checked_at", Value: time.Now().UTC().Format(time.RFC3339Nano)}, {Code: "connection.outcome", Value: "invalid_configuration"}, {Code: "connection.reason", Value: "invalid_configuration"}, {Code: "connection.server_source", Value: "unknown"}, {Code: "connection.control", Value: "not_connected"}, {Code: "connection.data_lane", Value: "unverified"}, {Code: "connection.phase.configuration", Value: "failed"}}}
		err = errors.New("invalid connection configuration")
	} else {
		creds := agentconn.Credentials{ServerURL: conf.Server, NodeToken: conf.Token, ConnectMode: conf.ConnectMode, Smoke: true}
		if conf.Validate() != nil {
			report = agentconn.New(creds, nil, nil, nil, log).ConnectionReport()
			putConnectionFact(&report, "outcome", "invalid_configuration")
			putConnectionFact(&report, "reason", "invalid_configuration")
			putConnectionFact(&report, "phase.configuration", "failed")
			putConnectionFact(&report, "control", "not_connected")
			err = errors.New("invalid connection configuration")
		} else {
			report, err = agentconn.DiagnoseConnectivity(ctx, creds, log)
		}
		putConnectionFact(&report, "server_source", connectionServerSource(launch, conf.Server, fromFile, os.Getenv))
		addConnectionHistory(&report, state, conf.Token)
	}
	renderConnectionReport(&report)
	if *asJSON {
		if writeErr := json.NewEncoder(out).Encode(report); writeErr != nil {
			return errors.New("cannot write connection report")
		}
	} else {
		if _, writeErr := io.WriteString(out, report.Report); writeErr != nil {
			return errors.New("cannot write connection report")
		}
	}
	return connectionExitError(err)
}

func connectionServerSource(launch *launchFlags, server string, fromFile bool, getenv func(string) string) string {
	specified := false
	launch.fs.Visit(func(f *flag.Flag) {
		if f.Name == "server" {
			specified = true
		}
	})
	if specified {
		return "flag"
	}
	if getenv("GPUWORKS_SERVER") != "" {
		return "environment"
	}
	if fromFile && server != "" {
		return "state_file"
	}
	return "missing"
}

func putConnectionFact(report *proto.DoctorReport, name, value string) {
	code := "connection." + name
	facts := report.Facts[:0]
	for _, fact := range report.Facts {
		if fact.Code != code {
			facts = append(facts, fact)
		}
	}
	report.Facts = append(facts, proto.DoctorFact{Code: code, Value: value})
}
func renderConnectionReport(report *proto.DoctorReport) {
	var text strings.Builder
	for _, fact := range report.Facts {
		if strings.HasPrefix(fact.Code, "connection.") {
			fmt.Fprintf(&text, "%s: %s\n", strings.TrimPrefix(fact.Code, "connection."), fact.Value)
		}
	}
	report.Report = text.String()
}

func connectionExitError(err error) error {
	if err == nil {
		return nil
	}
	code := 1
	if errors.Is(err, context.DeadlineExceeded) {
		code = 3
	} else {
		var reject *agentconn.RejectError
		if errors.As(err, &reject) {
			code = 4
		}
	}
	// New/legacy callers never print a remote detail or a transport URL.
	return &exitCodeError{code: code, err: errors.New("connection check failed")}
}

func smoke(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	launch := newLaunchFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "Usage of smoke:")
			fs.SetOutput(os.Stderr)
			fs.PrintDefaults()
			return err
		}
		return errors.New("invalid smoke arguments")
	}
	if fs.NArg() != 0 {
		return errors.New("smoke: unexpected argument")
	}
	conf, _, _, err := launch.resolve(os.Getenv)
	if err != nil {
		return errors.New("invalid connection configuration")
	}
	if conf.Server == "" || conf.Token == "" {
		return errors.New("--server and --token are required")
	}
	c := agentconn.New(agentconn.Credentials{ServerURL: conf.Server, NodeToken: conf.Token, Smoke: true}, nil, nil, nil, log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = c.Run(ctx)
	return connectionExitError(err)
}

func safeAgentConfigurationError(err error) error {
	for _, category := range []error{agentconf.ErrServerRequired, agentconf.ErrServerInvalid, agentconf.ErrTokenRequired, agentconf.ErrModeInvalid, agentconf.ErrListenInvalid, agentconf.ErrValueInvalid} {
		if errors.Is(err, category) {
			if category == agentconf.ErrServerInvalid {
				return errors.New("the server URL is invalid: it must start with http:// or https:// and contain no credentials, query or fragment")
			}
			return category
		}
	}
	return errors.New("cannot read agent configuration")
}
