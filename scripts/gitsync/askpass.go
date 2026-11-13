package main

import (
	"fmt"
	"os"
	"strings"
)

// git asks GIT_ASKPASS for HTTP credentials. The syncer points it at its own
// binary with these variables set for one git command, so a token never sits
// in a URL, in argv, or in a credential file on disk.
const (
	askpassEnv         = "GITSYNC_ASKPASS"
	askpassUserEnv     = "GITSYNC_ASKPASS_USER"
	askpassPasswordEnv = "GITSYNC_ASKPASS_PASSWORD"
)

// runAskpass answers git's prompt ("Username for 'https://…': " or
// "Password for 'https://user@…': ") and reports whether this process was
// started as the askpass hook.
func runAskpass(args []string) bool {
	if os.Getenv(askpassEnv) != "1" {
		return false
	}
	prompt := ""
	if len(args) > 1 {
		prompt = args[1]
	}
	if strings.HasPrefix(prompt, "Username") {
		fmt.Println(os.Getenv(askpassUserEnv))
	} else {
		fmt.Println(os.Getenv(askpassPasswordEnv))
	}
	return true
}
