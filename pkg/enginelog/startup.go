package enginelog

import (
	"math"
	"regexp"
	"strings"
)

var (
	vllmLoadingSecondsRE   = regexp.MustCompile(`(?i)\bModel loading took\s+[0-9,.]+\s*GiB\s+and\s+` + num + `\s+seconds?\b`)
	vllmCompileSecondsRE   = regexp.MustCompile(`(?i)\btorch\.compile takes\s+` + num + `\s+s(?:econds?)?\s+in total\b`)
	vllmGraphSecondsRE     = regexp.MustCompile(`(?i)\bGraph captur\w*\s+finished in\s+` + num + `\s+sec(?:ond)?s?\b`)
	sglangLoadingSecondsRE = regexp.MustCompile(`(?i)\bLoad weight end\.[^\r\n]*?\belapsed=\s*` + num + `\s+s\b`)
	sglangGraphSecondsRE   = regexp.MustCompile(`(?i)\bCapture[^\r\n]*?\bCUDA graph end\.[^\r\n]*?\b(?:elapsed=|Time elapsed:)\s*` + num + `\s+s\b`)
)

func parseStartupDurations(m *Measured, dialect, logText string) {
	if strings.EqualFold(strings.TrimSpace(dialect), DialectSGLang) {
		m.ModelLoadingSeconds = maxLoggedSeconds(sglangLoadingSecondsRE, logText)
		m.GraphCaptureSeconds = maxLoggedSeconds(sglangGraphSecondsRE, logText)
	} else {
		m.ModelLoadingSeconds = maxLoggedSeconds(vllmLoadingSecondsRE, logText)
		m.CompilationSeconds = maxLoggedSeconds(vllmCompileSecondsRE, logText)
		m.GraphCaptureSeconds = maxLoggedSeconds(vllmGraphSecondsRE, logText)
	}
}

func maxLoggedSeconds(re *regexp.Regexp, logText string) *float64 {
	var largest *float64
	for _, match := range re.FindAllStringSubmatch(logText, -1) {
		v, ok := toFloat(match[1])
		if !ok || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			continue
		}
		if largest == nil || v > *largest {
			value := v
			largest = &value
		}
	}
	return largest
}
