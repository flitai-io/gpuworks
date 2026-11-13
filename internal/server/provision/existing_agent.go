package provision

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/launchd"
)

const checkExistingAgent = "Existing agent"

// existingAgentCommand lists the servers of gpuworks agents already on a host:
// "unit <server>" for the gpuworks-agent systemd unit, or on a Mac the
// launchd job (every install writes that one service, so installing replaces
// it; a launch daemon's plist is root's alone, so its server reads empty) and
// "process <server> <state>" for each running agent, "-" for a server that
// cannot be read or that a server_dial agent does not have. Servers come from
// each agent's configuration file, or an earlier install's arguments
// (agentconf.ShellFuncs). Only servers and state directories are printed; the
// command lines carry tokens.
var existingAgentCommand = `set +e
` + agentconf.ShellFuncs + `
if command -v systemctl >/dev/null 2>&1 && systemctl cat gpuworks-agent >/dev/null 2>&1; then
  echo "unit $(gw_unit_value SERVER)"
fi
for plist in ` + shellQuote(launchd.DaemonPlist()) + ` "$HOME/` + launchd.AgentPlist("") + `"; do
  [ -e "$plist" ] || continue
  server=""
  if [ -r "$plist" ]; then
    server=$(sed -n 's|.*<string>--server</string><string>\([^<]*\)</string>.*|\1|p' "$plist" | head -n 1)
    if [ -z "$server" ]; then
      state=$(sed -n 's|.*<string>--state</string><string>\([^<]*\)</string>.*|\1|p' "$plist" | head -n 1)
      [ -n "$state" ] && server=$(gw_conf "$state" SERVER)
    fi
  fi
  echo "unit $server"
done
{ ps axww -o args= 2>/dev/null || ps axww 2>/dev/null; } | grep 'gpuworks-agent run' | grep -v grep | while IFS= read -r line; do
  server=$(gw_line_value "$line" SERVER)
  echo "process ${server:--} $(printf '%s\n' "$line" | gw_arg state)"
done
exit 0`

// ExistingAgent is what an install would replace or run beside on a host.
type ExistingAgent struct {
	// Unit is true when the gpuworks-agent systemd unit exists; UnitServer is
	// its --server, empty when it cannot be read. An install replaces the unit.
	Unit       bool
	UnitServer string
	// ProcessServers are the distinct servers of running agents, including the
	// one the unit runs.
	ProcessServers []string
	// ProcessStates are the distinct --state directories of running agents.
	ProcessStates []string
}

// Found reports whether the host already has a gpuworks agent.
func (e ExistingAgent) Found() bool {
	return e.Unit || len(e.ProcessServers) > 0 || len(e.ProcessStates) > 0
}

// Replaces reports whether installing for serverURL replaces an agent: the
// shared unit, or (without one) an agent for this server started by hand,
// which the rootless start stops. Agents for other servers outside the unit
// keep running.
func (e ExistingAgent) Replaces(serverURL string) bool {
	return e.Unit || slices.Contains(e.outsideUnit(), trimServer(serverURL))
}

// ReplacesNohup reports whether a nohup install into stateDir replaces an
// agent: only one running from that directory, which the start stops. The
// unit and agents in other directories keep running beside it.
func (e ExistingAgent) ReplacesNohup(stateDir string) bool {
	return slices.Contains(e.ProcessStates, strings.TrimRight(strings.TrimSpace(stateDir), "/"))
}

// outsideUnit are the servers of running agents the unit does not account for.
func (e ExistingAgent) outsideUnit() []string {
	var out []string
	for _, server := range e.ProcessServers {
		if !e.Unit || server != e.UnitServer {
			out = append(out, server)
		}
	}
	return out
}

func trimServer(server string) string { return strings.TrimRight(strings.TrimSpace(server), "/") }

func parseExistingAgent(out string) ExistingAgent {
	var found ExistingAgent
	for _, line := range strings.Split(out, "\n") {
		kind, server, _ := strings.Cut(strings.TrimSpace(line), " ")
		server = trimServer(server)
		switch kind {
		case "unit":
			found.Unit, found.UnitServer = true, server
		case "process":
			server, state, _ := strings.Cut(server, " ")
			if server = trimServer(server); server == "-" {
				server = ""
			}
			if server != "" && !slices.Contains(found.ProcessServers, server) {
				found.ProcessServers = append(found.ProcessServers, server)
			}
			if state = strings.TrimRight(strings.TrimSpace(state), "/"); state != "" && !slices.Contains(found.ProcessStates, state) {
				found.ProcessStates = append(found.ProcessStates, state)
			}
		}
	}
	sort.Strings(found.ProcessServers)
	sort.Strings(found.ProcessStates)
	return found
}

func detectExistingAgent(ctx context.Context, client *ssh.Client) (ExistingAgent, error) {
	out, err := runContext(ctx, client, existingAgentCommand)
	if err != nil {
		return ExistingAgent{}, fmt.Errorf("look for an existing agent: %s: %w", strings.TrimSpace(out), err)
	}
	return parseExistingAgent(out), nil
}

// existingAgentCheck is the precheck item: a warning when an install would
// replace an agent already on the host, above all one for another server.
func existingAgentCheck(ctx context.Context, client *ssh.Client, serverURL string) CheckResult {
	found, err := detectExistingAgent(ctx, client)
	if err != nil {
		return CheckResult{Name: checkExistingAgent, Status: "warn", Detail: err.Error()}
	}
	return CheckResult{Name: checkExistingAgent, Status: existingAgentStatus(found), Detail: existingAgentDetail(found, serverURL)}
}

func existingAgentStatus(found ExistingAgent) string {
	if found.Found() {
		return "warn"
	}
	return "ok"
}

func existingAgentDetail(found ExistingAgent, serverURL string) string {
	if !found.Found() {
		return "no gpuworks agent on the host"
	}
	own := trimServer(serverURL)
	var parts []string
	switch {
	case !found.Unit:
	case found.UnitServer == "":
		parts = append(parts, "a gpuworks-agent service whose server cannot be read is on the host; installing replaces it")
	case found.UnitServer == own:
		parts = append(parts, "the gpuworks-agent service on the host already runs for this server; installing replaces it and the node it belongs to goes offline")
	default:
		parts = append(parts, "the gpuworks-agent service on the host belongs to another server ("+found.UnitServer+"); installing replaces it and that server loses this host")
	}
	var others []string
	for _, server := range found.outsideUnit() {
		if server == own {
			parts = append(parts, "an agent for this server runs outside the gpuworks-agent service; installing replaces it")
		} else {
			others = append(others, server)
		}
	}
	if len(others) > 0 {
		parts = append(parts, "agents for other servers ("+strings.Join(others, ", ")+") run outside the gpuworks-agent service; installing leaves them running")
	}
	return strings.Join(parts, "; ")
}

// ExistingAgentError stops an install that would replace an agent already on
// the host until the caller confirms with ReplaceExistingAgent.
type ExistingAgentError struct {
	Found     ExistingAgent
	ServerURL string
	// InstallDir is set for a nohup install, which replaces only the agent
	// running from this directory.
	InstallDir string
}

func (e *ExistingAgentError) Error() string {
	if e.InstallDir != "" {
		return "a gpuworks agent already runs from " + e.InstallDir + "; installing there replaces it"
	}
	return existingAgentDetail(e.Found, e.ServerURL)
}
