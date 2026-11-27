package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMetaXProbeSurfacesSummaryFailure(t *testing.T) {
	p := &MetaXProber{hostRoot: t.TempDir(), lookPath: func(string) (string, error) { return "/usr/bin/mx-smi", nil }, run: func(_ context.Context, args ...string) (string, error) {
		if args[0] == "-L" {
			return "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\n", nil
		}
		return "", errors.New("summary unavailable")
	}}
	cards, err := p.Probe(t.Context())
	if err == nil {
		t.Fatalf("Probe accepted unknown capacity: %+v", cards)
	}
	if len(cards) != 0 {
		t.Fatalf("partial listing was published: %+v", cards)
	}
}

func TestMetaXProbeRejectsUnknownSummaryUnit(t *testing.T) {
	// The captured summary uses KB. Other units and absent units are
	// synthetic negative cases and cannot be interpreted as KB.
	summary := metaxFixture(t, "mx-smi-summary.txt")
	for _, unit := range []string{"MB", "bytes", ""} {
		t.Run(unit, func(t *testing.T) {
			changed := strings.ReplaceAll(summary, "67108864 KB", "67108864 "+unit)
			p := &MetaXProber{hostRoot: t.TempDir(), lookPath: func(string) (string, error) { return "mx-smi", nil }, run: func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "-L":
					return metaxFixture(t, "mx-smi-list.txt"), nil
				case "-s":
					return changed, nil
				default:
					return "", nil
				}
			}}
			cards, err := p.Probe(t.Context())
			if err == nil || len(cards) != 0 {
				t.Fatalf("unknown unit accepted: cards=%+v err=%v", cards, err)
			}
		})
	}
}

func TestMetaXProbeRejectsAmbiguousCapacity(t *testing.T) {
	const header = "GPU#0 MXC550 0000:0f:00.0\n"
	for name, summary := range map[string]string{
		"duplicate card":     header + "vram total : 1024 KB\n" + header + "vram total : 2048 KB\n",
		"duplicate total":    header + "vram total : 1024 KB\nvram total : 2048 KB\n",
		"invalid then total": header + "vram total : invalid KB\nvram total : 1024 KB\n",
		"overflow":           header + "vram total : 9223372036854775807 KB\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := &MetaXProber{hostRoot: t.TempDir(), lookPath: func(string) (string, error) { return "mx-smi", nil }, run: func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "-L":
					return "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\n", nil
				case "-s":
					return summary, nil
				default:
					return "", nil
				}
			}}
			cards, err := p.Probe(t.Context())
			if err == nil || len(cards) != 0 {
				t.Fatalf("ambiguous capacity accepted: cards=%+v err=%v", cards, err)
			}
		})
	}
}

func TestMetaXProbeRejectsListShorterThanSummary(t *testing.T) {
	p := &MetaXProber{hostRoot: t.TempDir(), lookPath: func(string) (string, error) { return "mx-smi", nil }, run: func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "-L":
			return "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\n", nil
		case "-s":
			return "GPU#0 MXC550 0000:0f:00.0\nvram total : 1024 KB\nGPU#1 MXC550 0000:34:00.0\nvram total : 1024 KB\n", nil
		default:
			return "", nil
		}
	}}
	if cards, err := p.Probe(t.Context()); err == nil || len(cards) != 0 {
		t.Fatalf("truncated list accepted: %+v, %v", cards, err)
	}
}

func TestMetaXProbeRejectsSummaryIndexDrift(t *testing.T) {
	p := &MetaXProber{hostRoot: t.TempDir(), lookPath: func(string) (string, error) { return "mx-smi", nil }, run: func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "-L":
			return "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\n", nil
		case "-s":
			return "GPU#1 MXC550 0000:0f:00.0\nvram total : 1024 KB\n", nil
		default:
			return "", nil
		}
	}}
	if cards, err := p.Probe(t.Context()); err == nil || len(cards) != 0 {
		t.Fatalf("summary identity drift accepted: %+v, %v", cards, err)
	}
}
