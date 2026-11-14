// Package logtext splits container output into display lines the way a
// terminal would show them. Progress bars (tqdm in vLLM/SGLang, pip, model
// downloaders) redraw in place with a bare carriage return and only end the
// line once they finish, so one "line" read up to '\n' holds every redraw.
// A terminal shows only the last one; so does this package. Server and agent
// share it, so it is a leaf with no repository dependencies.
package logtext

import (
	"bufio"
	"bytes"
	"io"
	"unicode/utf8"
)

// MaxLine bounds one line in bytes. A longer line is broken rather than
// failing the scanner: bufio.ErrTooLong would end the whole log stream.
const MaxLine = 1024 * 1024

// Collapse returns what a terminal leaves visible for one line: the text after
// the last carriage return. Trailing carriage returns (CRLF output, a redraw
// that ends exactly at the newline) are ignored first, so they never blank the
// line.
func Collapse(line string) string {
	end := len(line)
	for end > 0 && line[end-1] == '\r' {
		end--
	}
	line = line[:end]
	for i := len(line) - 1; i >= 0; i-- {
		if line[i] == '\r' {
			return line[i+1:]
		}
	}
	return line
}

func collapseBytes(line []byte) []byte {
	line = bytes.TrimRight(line, "\r")
	if i := bytes.LastIndexByte(line, '\r'); i >= 0 {
		return line[i+1:]
	}
	return line
}

// SplitLines is a bufio.SplitFunc that yields collapsed lines. While a line is
// still open and has reached maxLine bytes, redraws before its last carriage
// return are discarded (they could never be visible), so a progress bar that
// redraws for hours still fits; a line with no redraw is broken at maxLine.
func SplitLines(maxLine int) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, collapseBytes(data[:i]), nil
		}
		if atEOF {
			return len(data), collapseBytes(data), nil
		}
		if len(data) >= maxLine {
			if i := bytes.LastIndexByte(data, '\r'); i >= 0 {
				return i + 1, nil, nil
			}
			// Keep a multi-byte character whole: cut before the last one if
			// it is incomplete.
			n := maxLine
			start := n - 1
			for start > 0 && start > n-utf8.UTFMax && !utf8.RuneStart(data[start]) {
				start--
			}
			if start > 0 && !utf8.FullRune(data[start:n]) {
				n = start
			}
			return n, data[:n], nil
		}
		return 0, nil, nil
	}
}

// NewScanner returns a scanner over container output that yields collapsed
// lines of at most MaxLine bytes.
func NewScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	sc.Split(SplitLines(MaxLine))
	return sc
}
