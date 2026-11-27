package enginelog

import (
	"regexp"
	"strings"
)

// rootCauseScan bounds how far back RootCause looks. vLLM's workers print
// their tracebacks before the engine core and the API server print theirs,
// so the cause is well above the last line; a tensor-parallel engine repeats
// it once per rank.
const rootCauseScan = 1000

var (
	// A process tag engines put before every line of a multiprocess run:
	// "(Worker_TP2 pid=1907) ", "(EngineCore pid=1650) ", "(APIServer pid=1) ".
	processTagRE = regexp.MustCompile(`^\([A-Za-z][\w.-]*(?: pid=\d+)?\)\s*`)
	// A logger header after the tag: "ERROR 11-05 13:39:46 [core.py:1183] ".
	logHeaderRE = regexp.MustCompile(`^(?:DEBUG|INFO|WARNING|ERROR|CRITICAL)\s+\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2}(?:\.\d+)?\s+\[[^\]]*\]\s*`)
	// Accelerator engine builds also use a bracketed severity and PID fields.
	applicationLogHeaderRE = regexp.MustCompile(`^\[(?:DEBUG|INFO|WARNING|ERROR|CRITICAL)\]\s+\d{4}-\d{2}-\d{2}[- ]\d{2}:\d{2}:\d{2}(?:\.\d+)?\s+(?:\(PID:[^)]*\)\s*)?`)
	// Preserve both spellings emitted by engine builds; the code alone does
	// not imply a wrapper when its message describes a concrete failure.
	genericApplicationExceptionRE = regexp.MustCompile(`(?i)\bERR99999\s+UNKNOWN\s+appli(?:cation|caiton)\s+exception[.!]?\s*$`)
	// The line a Python traceback ends with: an exception class, optionally
	// dotted (torch.OutOfMemoryError), then its message.
	exceptionLineRE = regexp.MustCompile(`^(?:[A-Za-z_][\w]*\.)*[A-Za-z_]\w*(?:Error|Exception|Exit|Interrupt|Warning)(?::\s|$)`)
)

// wrapperCauses are exceptions engines raise to say a failure happened
// elsewhere: they point at the root cause instead of being it.
var wrapperCauses = []string{
	"see root cause above",
	"check the stack trace above for the root cause",
	"see stack trace for root cause",
	"engine core initialization failed",
	"enginecore failed to start",
	"workerproc initialization failed",
	"an exception in a background process",
	// SGLang's server on the signal a failed scheduler or detokenizer sends
	// it: the first while it is still launching, the second once it serves.
	"received sigquit from a child process",
	"sigquit received",
}

// RootCause is the line an engine's failure should be reported by. It is
// LastError, unless that line is one of the wrappers engines raise to point
// elsewhere ("Engine core initialization failed. See root cause above."):
// then it is the last exception above it that is not a wrapper, which in a
// multiprocess vLLM run is a worker's or the engine core's own exception.
// Process tags and logger headers are removed and the line is bounded. With
// no such exception, the generic application exception gives way to a known
// failure (Match) the engine logged as an error; otherwise the wrapper is
// what there is.
func RootCause(logText string) string {
	last := LastError(logText)
	if last == "" || !isWrapperCause(last) {
		return last
	}
	lines := strings.Split(ansiRE.ReplaceAllString(logText, ""), "\n")
	start := max(0, len(lines)-rootCauseScan)
	for i := len(lines) - 1; i >= start; i-- {
		line := stripLinePrefix(lines[i])
		if exceptionLineRE.MatchString(line) && !isWrapperCause(line) {
			return bound(line)
		}
	}
	// No exception stands above the wrapper. Under the generic application
	// exception, a known failure the engine logged as an error still says
	// more than the wrapper does: the most specific one's newest such line
	// is the error. Lines of a lower severity (a warning the engine got
	// past, an echo of its own arguments) are not a failure's own line, and
	// the wrappers that name where their cause is are left as they are.
	if genericApplicationExceptionRE.MatchString(last) {
		for _, cause := range Match(lines[start:]) {
			for i := len(cause.Evidence) - 1; i >= 0; i-- {
				raw := cause.Evidence[i].Line
				if line := stripLinePrefix(raw); line != "" && loggedAsError(raw) {
					return bound(line)
				}
			}
		}
	}
	return last
}

// errorSeverityRE is the severity a logger header states, after the process
// tag: "ERROR 11-05 13:39:46 [...]" or "[ERROR] 2026-09-18-20:39:28".
var errorSeverityRE = regexp.MustCompile(`^(?:\[(?:ERROR|CRITICAL)\]|(?:ERROR|CRITICAL)\b)`)

// loggedAsError reports whether the engine printed line at error severity.
func loggedAsError(line string) bool {
	line = processTagRE.ReplaceAllString(strings.TrimSpace(line), "")
	return errorSeverityRE.MatchString(line)
}

func stripLinePrefix(line string) string {
	line = strings.TrimSpace(line)
	line = processTagRE.ReplaceAllString(line, "")
	line = logHeaderRE.ReplaceAllString(line, "")
	return strings.TrimSpace(applicationLogHeaderRE.ReplaceAllString(line, ""))
}

func isWrapperCause(line string) bool {
	if genericApplicationExceptionRE.MatchString(line) {
		return true
	}
	text := strings.ToLower(line)
	for _, wrapper := range wrapperCauses {
		if strings.Contains(text, wrapper) {
			return true
		}
	}
	return false
}
