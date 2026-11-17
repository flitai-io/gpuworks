package aliyun

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provision"
)

// checkText is what a person reads of a check: its English fallback and
// the evidence behind it.
func checkText(c provision.CheckResult) string {
	return strings.TrimSpace(c.Message + " " + c.Evidence)
}
