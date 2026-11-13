package launchd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AgentProcesses independently observes agents using binary and state. A job
// absent from launchd can still have a process surviving a previous bootout.
// Raw process arguments never leave this observer, including in errors.
func (h *Host) AgentProcesses(ctx context.Context, binary, state string) ([]int, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out string
	var err error
	if h.ProcessTable != nil {
		out, err = h.ProcessTable(probeCtx)
	} else {
		var data []byte
		data, err = exec.CommandContext(probeCtx, "/bin/ps", "-ww", "-axo", "pid=,command=").Output()
		out = string(data)
	}
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, errors.New("agent process table could not be observed")
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		end := strings.IndexAny(line, " \t")
		if end < 0 {
			return nil, errors.New("agent process table is incomplete")
		}
		pid, err := strconv.Atoi(line[:end])
		if err != nil || pid <= 0 {
			return nil, errors.New("agent process table is incomplete")
		}
		if pid == os.Getpid() {
			continue
		}
		command := strings.TrimSpace(line[end:])
		if command == "" {
			return nil, errors.New("agent process table is incomplete")
		}
		for offset := 0; offset < len(command); {
			n := strings.Index(command[offset:], " run")
			if n < 0 {
				break
			}
			start := offset + n
			offset = start + len(" run")
			if offset < len(command) && command[offset] != ' ' && command[offset] != '\t' {
				continue
			}
			program := command[:start]
			if !filepath.IsAbs(program) {
				if filepath.Base(program) == filepath.Base(binary) {
					return nil, errors.New("relative agent process identity could not be established")
				}
				continue
			}
			if !sameAgentPath(program, binary) {
				continue
			}
			matches, err := agentProcessState(command[offset:], state)
			if err != nil {
				return nil, err
			}
			if matches {
				pids = append(pids, pid)
			}
			break
		}
	}
	return pids, nil
}

var processStateFlag = regexp.MustCompile(`(?:^|\s)--state(?:=|\s+)`)

func agentProcessState(args, state string) (bool, error) {
	matches := processStateFlag.FindAllStringIndex(args, -1)
	if len(matches) != 1 {
		return false, errors.New("agent process state is ambiguous")
	}
	value := strings.TrimSpace(args[matches[0][1]:])
	if value == state || strings.HasPrefix(value, state+" --") {
		return true, nil
	}
	fields := strings.Fields(value)
	if len(fields) == 0 || !filepath.IsAbs(fields[0]) || strings.ContainsAny(fields[0], "\"'\\") || (len(fields) > 1 && !strings.HasPrefix(fields[1], "--")) {
		return false, errors.New("agent process state could not be established")
	}
	return sameAgentPath(fields[0], state), nil
}
