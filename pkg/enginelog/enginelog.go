// Package enginelog parses inference-engine startup logs to extract the real
// measured memory breakdown, used to calibrate memory estimates.
//
// Parsing is intentionally resilient: unknown or changed log formats fail
// silently (fields are left zero, never an error), and dialect-specific regexp
// tables can grow without changing the measured JSON shape consumed by the UI.
package enginelog

import (
	"regexp"
	"strconv"
	"strings"
)

// Measured holds values parsed from engine startup logs. Zero means not found.
type Measured struct {
	ModelWeightsGiB float64 // model weight memory, when logged
	KVCacheGiB      float64 // KV cache memory, when logged
	KVCacheTokens   int     // KV cache size in tokens, when logged
	GraphCaptureGiB float64 // CUDA graph capture memory, when logged
	MaxConcurrency  float64 // maximum concurrency, when logged
	NumGPUBlocks    int     // GPU block count, when logged
	// Durations are the largest explicitly logged block across workers, not
	// sums of parallel ranks or overlapping initialization phases. Nil means
	// the log did not report that duration; a reported zero remains known.
	ModelLoadingSeconds *float64 `json:",omitempty"`
	CompilationSeconds  *float64 `json:",omitempty"`
	GraphCaptureSeconds *float64 `json:",omitempty"`
	// StartupSeconds is observed by the agent from the first health probe of
	// a newly started container to readiness. Log parsing never infers it.
	StartupSeconds *float64 `json:",omitempty"`
	// StartupTimeoutSeconds is the new container's observed health wait when
	// its budget expired without readiness. It is censored, not a completion
	// duration, and survives later retirement of the failed attempt.
	StartupTimeoutSeconds *float64 `json:",omitempty"`
	Found                 bool     // true if a memory/cache figure was parsed; durations have their own presence
}

type pattern struct {
	name string
	re   *regexp.Regexp
	set  func(m *Measured, capture string) bool
}

func toFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func toInt(s string) (int, bool) {
	n, err := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	if err != nil {
		return 0, false
	}
	return n, true
}

func setFloat(get func(*Measured) *float64) func(*Measured, string) bool {
	return func(m *Measured, capture string) bool {
		p := get(m)
		if *p != 0 {
			return false
		}
		v, ok := toFloat(capture)
		if !ok || v == 0 {
			return false
		}
		*p = v
		return true
	}
}

func setInt(get func(*Measured) *int) func(*Measured, string) bool {
	return func(m *Measured, capture string) bool {
		p := get(m)
		if *p != 0 {
			return false
		}
		v, ok := toInt(capture)
		if !ok || v == 0 {
			return false
		}
		*p = v
		return true
	}
}

const num = `([0-9][0-9,]*(?:\.[0-9]+)?)`

var vllmPatterns = []pattern{
	{
		name: "model-loading-took",
		re:   regexp.MustCompile(`(?i)model loading took\s+` + num + `\s*GiB`),
		set:  setFloat(func(m *Measured) *float64 { return &m.ModelWeightsGiB }),
	},
	{
		name: "model-weights-take",
		re:   regexp.MustCompile(`(?i)the model weights take\s+` + num + `\s*GiB`),
		set:  setFloat(func(m *Measured) *float64 { return &m.ModelWeightsGiB }),
	},
	{
		name: "available-kv-cache-memory",
		re:   regexp.MustCompile(`(?i)available KV cache memory:\s*` + num + `\s*GiB`),
		set:  setFloat(func(m *Measured) *float64 { return &m.KVCacheGiB }),
	},
	{
		name: "reserved-for-kv-cache-is",
		re:   regexp.MustCompile(`(?i)reserved for KV Cache is\s+` + num + `\s*GiB`),
		set:  setFloat(func(m *Measured) *float64 { return &m.KVCacheGiB }),
	},
	{
		name: "gpu-kv-cache-gib",
		re:   regexp.MustCompile(`(?i)GPU KV cache:\s*` + num + `\s*GiB`),
		set:  setFloat(func(m *Measured) *float64 { return &m.KVCacheGiB }),
	},
	{
		name: "gpu-kv-cache-size-tokens",
		re:   regexp.MustCompile(`(?i)GPU KV cache size:\s*` + num + `\s*tokens`),
		set:  setInt(func(m *Measured) *int { return &m.KVCacheTokens }),
	},
	{
		name: "graph-capturing-took",
		re:   regexp.MustCompile(`(?i)graph captur\w*\s+finished.*?took\s+` + num + `\s*GiB`),
		set:  setFloat(func(m *Measured) *float64 { return &m.GraphCaptureGiB }),
	},
	{
		name: "maximum-concurrency",
		re:   regexp.MustCompile(`(?i)maximum concurrency for\s+[0-9,]+\s+tokens per request:\s*` + num + `x`),
		set:  setFloat(func(m *Measured) *float64 { return &m.MaxConcurrency }),
	},
	{
		name: "gpu-blocks",
		re:   regexp.MustCompile(`(?i)#?\s*GPU blocks:\s*` + num),
		set:  setInt(func(m *Measured) *int { return &m.NumGPUBlocks }),
	},
}

var glibcMissingRE = regexp.MustCompile("(?i)(?:version [`']?)?(GLIBC_[0-9]+(?:\\.[0-9]+)+)(?:[`']? not found|[^\\n]*required)")

// HumanizeStartupFailure maps dynamic-loader failures that otherwise look like
// an opaque exit 127 into an operator-facing partition compatibility reason.
func HumanizeStartupFailure(logText string) string {
	match := glibcMissingRE.FindStringSubmatch(logText)
	if len(match) > 1 {
		return "container glibc too old: missing " + strings.ToUpper(match[1]) + "; Ascend dGPU images require glibc 2.33 or newer"
	}
	return ""
}

// hamiOOMRE is the line HAMi-core (the NVIDIA dGPU interposer) prints when it
// refuses an allocation because it would take the instance past its memory
// limit: "Device <cuda index> OOM <bytes the instance would hold> / <limit>".
var hamiOOMRE = regexp.MustCompile(`HAMI-core ERROR[^\n]*?Device (\d+) OOM (\d+) / (\d+)`)

// DGPULimitExceeded says, in words, that an engine ran into its dGPU memory
// limit, from the last such refusal in its output; empty when there is none.
// The engine's own report of the refusal is a generic error raised far from
// it (a 500 to a request, or an initialization failure), so without this the
// failure names a symptom and not the limit.
func DGPULimitExceeded(logText string) string {
	matches := hamiOOMRE.FindAllStringSubmatch(logText, -1)
	if len(matches) == 0 {
		return ""
	}
	m := matches[len(matches)-1]
	wanted, _ := strconv.ParseFloat(m[2], 64)
	limit, _ := strconv.ParseFloat(m[3], 64)
	const gib = 1 << 30
	return "dGPU memory limit reached on GPU " + m[1] + ": the engine needed " +
		strconv.FormatFloat(wanted/gib, 'f', 1, 64) + " GiB, its limit is " +
		strconv.FormatFloat(limit/gib, 'f', 1, 64) + " GiB"
}

var (
	ansiRE      = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	errorLineRE = regexp.MustCompile(`(?i)(^|[\s:])[a-z_.]*(error|exception)\b|\berror:`)
)

// lastErrorScan bounds how far back LastError looks for an error line.
const lastErrorScan = 50

// lastErrorMax bounds the line LastError returns.
const lastErrorMax = 300

// LastError is the line an engine's output ends its failure with: the last
// line naming an error or exception among its final lines, empty when there
// is none (an engine killed while serving ends on an ordinary line, which says
// nothing about why). Terminal control sequences are removed and the line is
// bounded, so it can stand in a failure message.
func LastError(logText string) string {
	lines := strings.Split(ansiRE.ReplaceAllString(logText, ""), "\n")
	scanned := 0
	for i := len(lines) - 1; i >= 0 && scanned < lastErrorScan; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		scanned++
		if errorLineRE.MatchString(line) {
			return bound(line)
		}
	}
	return ""
}

func bound(line string) string {
	if r := []rune(line); len(r) > lastErrorMax {
		return string(r[:lastErrorMax]) + "…"
	}
	return line
}

// Dialect names. pkg cannot import internal, so these are defined here rather
// than taken from internal/engines; the two lists are held equal by
// TestEngineLogDialectsMatchEngines in internal/server/deploy.
const (
	DialectVLLM   = "vllm"
	DialectSGLang = "sglang"
)

// Dialects returns every log dialect this package can parse, in sorted order.
func Dialects() []string { return []string{DialectSGLang, DialectVLLM} }

// Parse scans log text using the default vLLM dialect.
func Parse(logText string) Measured {
	return ParseDialect(DialectVLLM, logText)
}

// ParseDialect scans log text using a dialect-specific table. Unknown dialects
// fall back to vLLM for backward compatibility with older specs.
func ParseDialect(dialect, logText string) Measured {
	var m Measured
	if strings.ToLower(strings.TrimSpace(dialect)) == DialectSGLang {
		m = parseSGLang(logText)
	} else {
		m = parseWith(vllmPatterns, logText)
	}
	parseStartupDurations(&m, dialect, logText)
	return m
}

func parseWith(patterns []pattern, logText string) Measured {
	var m Measured
	for _, p := range patterns {
		for _, match := range p.re.FindAllStringSubmatch(logText, -1) {
			if len(match) < 2 {
				continue
			}
			if p.set(&m, match[1]) {
				m.Found = true
				break
			}
		}
	}
	return m
}
