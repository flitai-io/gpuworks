package logtext

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

func TestCollapse(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain line", "plain line"},
		{"", ""},
		{"crlf\r", "crlf"},
		{"\t indented\tcolumns", "\t indented\tcolumns"},
		{"a  0%|   |\ra 50%|█  |\ra100%|███|", "a100%|███|"},
		// tqdm's close() redraws the final frame and then writes '\n'; the
		// redraw that ends exactly at the line end must not blank the line.
		{"a 50%|█  |\ra100%|███|\r", "a100%|███|"},
		{"\r\r", ""},
		{"\x1b[32mINFO\x1b[0m ready", "\x1b[32mINFO\x1b[0m ready"},
	}
	for _, tc := range cases {
		if got := Collapse(tc.in); got != tc.want {
			t.Errorf("Collapse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func scanAll(t *testing.T, sc *bufio.Scanner) []string {
	t.Helper()
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return lines
}

func equalLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q (all: %q)", i, got[i], want[i], got)
		}
	}
}

// The literal shape of vLLM's CUDA-graph capture: log lines on stdout, tqdm
// redraws on stderr joined by '\r', one '\n' when the bar closes.
func TestScannerKeepsTheFinalProgressFrame(t *testing.T) {
	var frames []string
	for i := 0; i <= 51; i += 2 {
		frames = append(frames, "Capturing CUDA graphs (PIECEWISE):  xx%|▏         | "+strings.Repeat("█", i/5)+" [00:01<00:52,  1.06s/it]")
	}
	frames = append(frames, "Capturing CUDA graphs (PIECEWISE): 100%|██████████| 51/51 [00:07<00:00,  6.61it/s]")
	input := "(EngineCore pid=192) INFO 11-14 13:36:15 [monitor.py:81] Initial profiling/warmup run took 0.12 s\n" +
		strings.Join(frames, "\r") + "\n" +
		"(EngineCore pid=192) INFO 11-14 13:36:26 [model_runner.py:1087] Graph capturing finished in 9 secs\r\n" +
		"partial last line without newline"
	// One byte per read: a line that arrives in pieces must collapse the same.
	got := scanAll(t, NewScanner(iotest.OneByteReader(strings.NewReader(input))))
	equalLines(t, got, []string{
		"(EngineCore pid=192) INFO 11-14 13:36:15 [monitor.py:81] Initial profiling/warmup run took 0.12 s",
		"Capturing CUDA graphs (PIECEWISE): 100%|██████████| 51/51 [00:07<00:00,  6.61it/s]",
		"(EngineCore pid=192) INFO 11-14 13:36:26 [model_runner.py:1087] Graph capturing finished in 9 secs",
		"partial last line without newline",
	})
}

func TestScannerKeepsEmptyLines(t *testing.T) {
	got := scanAll(t, NewScanner(strings.NewReader("a\n\n\r\nb\n")))
	equalLines(t, got, []string{"a", "", "", "b"})
}

// A bar that redraws for hours grows its open line past the bound; the old
// redraws are dropped as they pile up, the stream keeps going, and only the
// final frame is shipped.
func TestScannerDropsRedrawsOfALongOpenLine(t *testing.T) {
	const maxLine = 64
	var b strings.Builder
	for i := range 1000 {
		b.WriteString("download " + strings.Repeat("#", i%40) + "\r")
	}
	b.WriteString("download done\nnext\n")
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	sc.Buffer(make([]byte, 16), maxLine)
	sc.Split(SplitLines(maxLine))
	equalLines(t, scanAll(t, sc), []string{"download done", "next"})
}

// A line longer than the bound with no redraw is broken, not fatal: the
// default scanner fails with bufio.ErrTooLong, which ends the log stream.
func TestScannerBreaksALongLineWithoutRedraws(t *testing.T) {
	const maxLine = 16
	sc := bufio.NewScanner(strings.NewReader(strings.Repeat("x", 40) + "\nafter\n"))
	sc.Buffer(make([]byte, 4), maxLine)
	sc.Split(SplitLines(maxLine))
	equalLines(t, scanAll(t, sc), []string{strings.Repeat("x", 16), strings.Repeat("x", 16), strings.Repeat("x", 8), "after"})
}

func TestScannerBreaksALongLineOnACharacterBoundary(t *testing.T) {
	const maxLine = 16
	// 15 ASCII bytes then a 3-byte character straddling the bound.
	input := strings.Repeat("a", 15) + "显存不足\n"
	sc := bufio.NewScanner(strings.NewReader(input))
	sc.Buffer(make([]byte, 4), maxLine)
	sc.Split(SplitLines(maxLine))
	got := scanAll(t, sc)
	if strings.Join(got, "") != strings.TrimSuffix(input, "\n") {
		t.Fatalf("pieces %q do not rejoin to the input", got)
	}
	for _, piece := range got {
		if !utf8.ValidString(piece) || len(piece) > maxLine {
			t.Fatalf("piece %q is not a whole-character chunk within %d bytes", piece, maxLine)
		}
	}
}

func TestScannerSurfacesReadErrors(t *testing.T) {
	boom := errors.New("stream reset")
	sc := NewScanner(io.MultiReader(strings.NewReader("done\nhalf"), iotest.ErrReader(boom)))
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if !errors.Is(sc.Err(), boom) {
		t.Fatalf("Err = %v, want %v", sc.Err(), boom)
	}
	equalLines(t, got, []string{"done", "half"})
}
