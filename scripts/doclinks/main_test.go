package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	root := t.TempDir()
	write(t, root, "README.md", "[ok](docs/a.md#第二节-details) [gone](docs/missing.md)\n")
	write(t, root, "CONTEXT.md", "term [[glossary ref]] is allowed here\n")
	write(t, root, "docs/a.md", "# A\n\n## 第二节 Details\n\n"+
		"[bad anchor](b.md#nope) [good](b.md#real-one) [self](#第二节-details) [web](https://x.test/y.md)\n"+
		"[private](../artifacts/work/absent/plan.md#x) [private dir](../artifacts/)\n"+
		"[wiki]: [[Term]]\n"+
		"`[code](nowhere.md)` and\n```\n[fenced](nowhere.md) [[Fenced]]\n```\n")
	write(t, root, "docs/b.md", "# B\n\n## Real one\n\n```\n# not a heading\n```\n")

	got, err := check(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"README.md: broken link docs/missing.md",
		"docs/a.md: missing anchor b.md#nope",
		"docs/a.md: wiki-style reference [[Term]] (use a normal Markdown link)",
	}
	joined := strings.Join(got, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("missing problem %q in:\n%s", w, joined)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d problems, want %d:\n%s", len(got), len(want), joined)
	}
}

func TestCheckEnglishPagesUseTheSitesAnchors(t *testing.T) {
	root := t.TempDir()
	write(t, root, "docs/reference/gateway.md", "# Gateway\n\n## 认证\n\n## 副本选择\n\n### 副本选择\n")
	write(t, root, "docs-en/reference/gateway.md", "# Gateway\n\n## Authentication\n\n## Replica Selection\n\n### Replica Selection\n")
	write(t, root, "docs/reference/only-zh.md", "# 只有中文\n\n## 第一节\n")
	write(t, root, "docs-en/reference/only-en.md", "# English only\n\n## Own slug\n")
	write(t, root, "docs/en-adopter.md", "# Adopter\n")
	write(t, root, "docs-en/en-adopter.md", "# Adopter\n\n[good](reference/gateway.md#认证) [dup](reference/gateway.md#副本选择-1) "+
		"[english slug](reference/gateway.md#authentication) [own](reference/only-en.md#own-slug) [self](../docs-en/en-adopter.md#adopter)\n")

	got, err := check(root)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	want := []string{"docs-en/en-adopter.md: missing anchor reference/gateway.md#authentication"}
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("missing problem %q in:\n%s", w, joined)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d problems, want %d:\n%s", len(got), len(want), joined)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"网络监控":                        "网络监控",
		"Real one":                    "real-one",
		"`gpuworks` 子命令 (v2)":         "gpuworks-子命令-v2",
		"操作指南（how-to）":                "操作指南how-to",
		"Register {#register-a-host}": "register-a-host",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q)=%q want %q", in, got, want)
		}
	}
}
