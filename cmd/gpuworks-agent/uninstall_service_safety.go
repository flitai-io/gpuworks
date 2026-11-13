package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (u linuxUninstaller) validateUnit(unit []byte) error {
	var starts []string
	for _, line := range strings.Split(string(unit), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			starts = append(starts, value)
		}
	}
	if len(starts) != 1 {
		return errors.New("agent service executable identity is ambiguous")
	}
	match := regexp.MustCompile(`^(?:"([^"]+)"|'([^']+)'|([^\s]+))\s+run(?:\s|$)`).FindStringSubmatch(starts[0])
	if len(match) == 0 {
		return errors.New("agent service executable identity is unrecognized")
	}
	binary := ""
	for _, value := range match[1:] {
		if value != "" {
			binary = strings.ReplaceAll(value, "%%", "%")
		}
	}
	if !sameAgentBinary(binary, u.binary) {
		return errors.New("agent service executable differs from this agent")
	}
	entries, err := os.ReadDir(u.unitPath + ".d")
	if err != nil && !os.IsNotExist(err) {
		return errors.New("agent service overrides could not be inspected")
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".conf") {
			return errors.New("agent service overrides require manual ownership verification")
		}
	}
	// An installed unit may have administrator-supplied global overrides.
	if filepath.IsAbs(u.unitPath) && filepath.Dir(u.unitPath) == "/etc/systemd/system" {
		for _, root := range []string{"/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
			entries, err := os.ReadDir(filepath.Join(root, "gpuworks-agent.service.d"))
			if err != nil && !os.IsNotExist(err) {
				return errors.New("agent service overrides could not be inspected")
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".conf") {
					return errors.New("agent service overrides require manual ownership verification")
				}
			}
		}
	}
	return nil
}
