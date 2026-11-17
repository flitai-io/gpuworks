package aihelper

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// Finding is a known engine failure a failed instance shows. Code is an
// enginelog failure code: the client renders the title and the suggested fix
// from it in its own locale. The LLM never sees a finding as a fact - it gets
// the code's hypothesis to verify against the evidence (see SeedHypotheses).
type Finding struct {
	Code     string     `json:"code"`
	Evidence []Evidence `json:"evidence"`
}

// Evidence is one line that showed the failure. Index is the line's position
// in the slice Match was given, so a caller can point back into its own view;
// a line the agent reported has none.
type Evidence struct {
	Index int    `json:"index"`
	Line  string `json:"line"`
}

// Findings is what a failed instance shows: the causes its agent reported
// with the failure when it reported a structured reason, else (an agent that
// predates them, or a row written before them) what the known-failure table
// finds in its lines. Codes are read in the one spelling.
func Findings(failure *proto.RuntimeFailure, lines []string) []Finding {
	if failure == nil {
		return Match(lines)
	}
	var out []Finding
	for _, c := range failure.Normalized().Causes {
		f := Finding{Code: c.Code, Evidence: make([]Evidence, len(c.Evidence))}
		for i, line := range c.Evidence {
			f.Evidence[i] = Evidence{Index: -1, Line: line}
		}
		out = append(out, f)
	}
	return out
}

// CauseFallback is the English fallback sentence for a cause an agent
// reported, for the product message the console renders from its code.
func CauseFallback(c proto.FailureCause) string {
	cause := enginelog.Cause{Code: c.Code, GLIBC: c.GLIBC, NeededBytes: c.NeededBytes, LimitBytes: c.LimitBytes}
	if c.Device != nil {
		cause.Device = *c.Device
	}
	return cause.Fallback()
}

// SignatureCodes lists every code a finding can carry.
func SignatureCodes() []string { return enginelog.Codes() }

// Match runs the known-failure table (pkg/enginelog, the only one) over
// lines, oldest first.
func Match(lines []string) []Finding {
	var out []Finding
	for _, c := range enginelog.Match(lines) {
		f := Finding{Code: c.Code, Evidence: make([]Evidence, len(c.Evidence))}
		for i, e := range c.Evidence {
			f.Evidence[i] = Evidence{Index: e.Index, Line: e.Line}
		}
		out = append(out, f)
	}
	return out
}

// SeedHypotheses renders findings for the LLM as hypotheses to verify, never
// as conclusions: a rule that misfires must not become the answer.
func SeedHypotheses(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Rule-based hypotheses (unverified - confirm or reject each against the evidence and say which):\n")
	for _, f := range findings {
		hypothesis := enginelog.Hypothesis(f.Code)
		if hypothesis == "" {
			continue
		}
		b.WriteString("- [" + f.Code + "] " + hypothesis + "\n")
		for _, e := range f.Evidence {
			b.WriteString("    evidence: " + clip(e.Line, maxLineChars) + "\n")
		}
	}
	return b.String()
}

// maxLineChars bounds one log line handed to the model; a minified JSON blob
// or a progress bar redrawn on one line is otherwise unbounded.
const maxLineChars = 400

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
