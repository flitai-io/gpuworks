package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/agentconf"
)

// launchFlags declares the flags that override the configuration file, the
// same set on run, smoke and upgrade, so each reads its configuration one
// way (resolve).
type launchFlags struct {
	fs    *flag.FlagSet
	conf  agentconf.Config
	state string
}

func newLaunchFlags(fs *flag.FlagSet) *launchFlags {
	l := &launchFlags{fs: fs}
	fs.StringVar(&l.state, "state", "", "state directory; its "+agentconf.FileName+" holds the configuration below")
	fs.StringVar(&l.conf.Server, "server", "", "server base URL (https://host:port); not used when the server dials the agent")
	fs.StringVar(&l.conf.Token, "token", "", "per-node connection token")
	fs.StringVar(&l.conf.ConnectMode, "connect-mode", "", "connect mode: agent_dial | server_dial")
	fs.StringVar(&l.conf.Listen, "listen", "", "server_dial listen address (default "+agentconf.DefaultListen+")")
	fs.StringVar(&l.conf.RuntimeDriver, "driver", "", "force runtime driver: docker | udocker | nerdctl | host | kube | none")
	return l
}

// launchEnv names the environment variables that override the file, flag by
// flag. They predate the file; a flag still beats them.
var launchEnv = []struct {
	name string
	ptr  func(*agentconf.Config) *string
}{
	{"GPUWORKS_SERVER", func(c *agentconf.Config) *string { return &c.Server }},
	{"GPUWORKS_TOKEN", func(c *agentconf.Config) *string { return &c.Token }},
	{"GPUWORKS_CONNECT_MODE", func(c *agentconf.Config) *string { return &c.ConnectMode }},
	{"GPUWORKS_SERVER_DIAL_LISTEN", func(c *agentconf.Config) *string { return &c.Listen }},
	{"GPUWORKS_RUNTIME_DRIVER", func(c *agentconf.Config) *string { return &c.RuntimeDriver }},
	{"GPUWORKS_RUNTIME_MOUNT_ROOT", func(c *agentconf.Config) *string { return &c.MountRoot }},
	{"GPUWORKS_UDOCKER_BUNDLE", func(c *agentconf.Config) *string { return &c.UdockerBundle }},
}

// resolve is the configuration an agent command runs with: the state
// directory's file, overridden by the environment, overridden by flags given
// on the command line. A missing file is no error: agents installed before it
// existed carry everything as flags. fromFile reports whether the file was
// read.
func (l *launchFlags) resolve(getenv func(string) string) (conf agentconf.Config, stateDir string, fromFile bool, err error) {
	stateDir = l.state
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	conf, err = agentconf.Load(stateDir)
	switch {
	case err == nil:
		fromFile = true
	case errors.Is(err, os.ErrNotExist):
		conf = agentconf.Config{}
	default:
		return agentconf.Config{}, "", false, fmt.Errorf("read %s: %w", agentconf.Path(stateDir), err)
	}
	for _, env := range launchEnv {
		if v := getenv(env.name); v != "" {
			*env.ptr(&conf) = v
		}
	}
	l.fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "server":
			conf.Server = l.conf.Server
		case "token":
			conf.Token = l.conf.Token
		case "connect-mode":
			conf.ConnectMode = l.conf.ConnectMode
		case "listen":
			conf.Listen = l.conf.Listen
		case "driver":
			conf.RuntimeDriver = l.conf.RuntimeDriver
		}
	})
	return conf.Normalized(), stateDir, fromFile, nil
}

// configure writes the state directory's configuration file, the one place
// an install records how the agent connects; the token is read from stdin so
// it is not an argument ps shows. The file is replaced whole.
func configure(args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	var c agentconf.Config
	state := fs.String("state", "", "state directory (required)")
	fs.StringVar(&c.Server, "server", "", "server base URL; required unless the server dials the agent")
	fs.StringVar(&c.ConnectMode, "connect-mode", agentconf.AgentDial, "connect mode: agent_dial | server_dial")
	fs.StringVar(&c.Listen, "listen", "", "server_dial listen address (default "+agentconf.DefaultListen+")")
	fs.StringVar(&c.RuntimeDriver, "driver", "", "force runtime driver")
	fs.StringVar(&c.MountRoot, "mount-root", "", "runtime mount root")
	fs.StringVar(&c.UdockerBundle, "udocker-bundle", "", "offline udocker bundle directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(*state) == "" {
		return fmt.Errorf("--state is required")
	}
	token, err := bufio.NewReader(stdin).ReadString('\n')
	if c.Token = strings.TrimSpace(token); c.Token == "" {
		return fmt.Errorf("the node token is read from stdin and none was given: %v", err)
	}
	if err := agentconf.Write(*state, c); err != nil {
		return err
	}
	fmt.Fprintf(out, "gpuworks: wrote %s\n", agentconf.Path(*state))
	return nil
}
