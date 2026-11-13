package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

func agentTCPListenerChecklist(procDir string, pids []int) uninstallcheck.Item {
	item := uninstallcheck.Item{ID: "agent.listeners.observation", Status: uninstallcheck.Pass, Summary: "No TCP listeners observed on matching agent processes", Action: "Only agent-held TCP sockets are observed; workload and unrelated ports are retained. Verify release again after shutdown."}
	ports := map[string]bool{}
	for _, pid := range pids {
		dir := filepath.Join(procDir, strconv.Itoa(pid))
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			item.Status, item.Summary = uninstallcheck.Unknown, "Matching agent TCP listener inventory is incomplete"
			continue
		}
		inodes := map[string]bool{}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
			if err != nil {
				if !os.IsNotExist(err) {
					item.Status, item.Summary = uninstallcheck.Unknown, "Matching agent TCP listener inventory is incomplete"
				}
				continue
			}
			if inode, ok := strings.CutPrefix(link, "socket:["); ok {
				inodes[strings.TrimSuffix(inode, "]")] = true
			}
		}
		for _, name := range []string{"tcp", "tcp6"} {
			data, err := os.ReadFile(filepath.Join(dir, "net", name))
			if os.IsNotExist(err) && name == "tcp6" {
				continue
			}
			if err != nil {
				item.Status, item.Summary = uninstallcheck.Unknown, "Matching agent TCP listener inventory is incomplete"
				continue
			}
			for _, line := range strings.Split(string(data), "\n")[1:] {
				fields := strings.Fields(line)
				if len(fields) < 10 || fields[3] != "0A" || !inodes[fields[9]] {
					continue
				}
				_, port, ok := strings.Cut(fields[1], ":")
				n, err := strconv.ParseUint(port, 16, 16)
				if ok && err == nil {
					ports["PID "+strconv.Itoa(pid)+"; TCP port "+strconv.FormatUint(n, 10)] = true
				}
			}
		}
	}
	for port := range ports {
		item.Details = append(item.Details, port)
	}
	sort.Strings(item.Details)
	if len(item.Details) > 0 && item.Status != uninstallcheck.Unknown {
		item.Status, item.Summary = uninstallcheck.Warn, "TCP listeners observed on matching agent processes"
	}
	return item
}
