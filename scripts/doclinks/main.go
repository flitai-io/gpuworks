// Command doclinks checks that relative Markdown links in the user-facing
// docs (docs/**, docs-en/**, README.md, README.zh-CN.md, CONTEXT.md,
// CLAUDE.md) resolve to real files and, when they carry a #fragment, to a real
// heading in the target file. Under docs/ it also rejects [[wiki]] style cross
// references, which neither GitHub nor the Hugo site renders as links.
// CONTEXT.md is exempt: it is the domain glossary and uses [[term]] as its own
// cross-reference convention.
//
// A link from a docs-en/ page is checked against the anchors the documentation
// site gives the headings, not against the English heading's own text: the
// site keeps the Chinese page's anchors on its English translation
// (docs-site/stage_docs.py preserve_heading_ids), so #认证 is the anchor of the
// section headed "Authentication" there. A target that docs/ does not carry is
// an untranslated page the site never builds; its English headings are used.
//
// Links into artifacts/ are not checked: it is a separate private repository
// that may be absent and is versioned on its own; scripts/artifactsindex
// reports stale references into it when it is checked out.
//
// Usage:
//
//	go run ./scripts/doclinks [-repo .]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var (
	linkRe       = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	wikiRe       = regexp.MustCompile(`\[\[[^\]]+\]\]`)
	headingRe    = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)
	fenceRe      = regexp.MustCompile("(?s)```.*?```")
	inlineRe     = regexp.MustCompile("`[^`\n]*`")
	explicitIDRe = regexp.MustCompile(`\s+\{#([^}]+)\}\s*$`)
)

func main() {
	repo := flag.String("repo", ".", "repository root")
	flag.Parse()
	problems, err := check(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doclinks:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "doclinks: %d problem(s)\n", len(problems))
		os.Exit(1)
	}
}

func scope(repo string) ([]string, error) {
	files := []string{}
	for _, f := range []string{"README.md", "README.zh-CN.md", "CONTEXT.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err == nil {
			files = append(files, f)
		}
	}
	for _, dir := range []string{"docs", "docs-en"} {
		err := filepath.WalkDir(filepath.Join(repo, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) && d == nil {
					return nil
				}
				return err
			}
			if !d.IsDir() && strings.HasSuffix(p, ".md") {
				rel, _ := filepath.Rel(repo, p)
				files = append(files, rel)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

// stripCode blanks fenced and inline code so that code samples containing
// "](" or "[[" are not mistaken for links.
func stripCode(s string) string {
	s = fenceRe.ReplaceAllString(s, "")
	return inlineRe.ReplaceAllString(s, "")
}

// slug follows GitHub's heading anchor rules closely enough for CJK headings:
// lower-case, keep letters/digits (CJK included), drop punctuation and symbols
// (full-width ones too) except '-' and '_', spaces become '-'.
func slug(h string) string {
	h = strings.TrimSpace(h)
	if m := explicitIDRe.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	h = inlineRe.ReplaceAllStringFunc(h, func(s string) string { return strings.Trim(s, "`") })
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(h)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_':
			b.WriteRune(r)
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// siteAnchors is the anchors the documentation site gives a page's headings:
// the slug of each heading, with duplicate slugs numbered -1, -2, ... in page
// order, and an explicit {#id} used as written.
func siteAnchors(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	seen := map[string]int{}
	for _, m := range headingRe.FindAllStringSubmatch(fenceRe.ReplaceAllString(string(raw), ""), -1) {
		a := slug(m[1])
		if n := seen[a]; n > 0 {
			a = fmt.Sprintf("%s-%d", a, n)
		}
		seen[slug(m[1])]++
		out[a] = true
	}
	return out, nil
}

// targetAnchors is the anchor set a link into target must name. A docs-en
// page keeps its Chinese page's anchors (see the package comment), so those
// come from the counterpart under docs/ when it exists.
func targetAnchors(repo, target string) (map[string]bool, error) {
	rel, err := filepath.Rel(repo, target)
	if err != nil {
		return siteAnchors(target)
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "docs-en/") {
		source := filepath.Join(repo, "docs", strings.TrimPrefix(rel, "docs-en/"))
		if _, err := os.Stat(source); err == nil {
			return siteAnchors(source)
		}
	}
	return siteAnchors(target)
}

func check(repo string) ([]string, error) {
	files, err := scope(repo)
	if err != nil {
		return nil, err
	}
	var problems []string
	cache := map[string]map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			return nil, err
		}
		body := stripCode(string(raw))
		if strings.HasPrefix(f, "docs"+string(filepath.Separator)) {
			for _, w := range wikiRe.FindAllString(body, -1) {
				problems = append(problems, fmt.Sprintf("%s: wiki-style reference %s (use a normal Markdown link)", f, w))
			}
		}
		for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			pathPart, frag, _ := strings.Cut(target, "#")
			resolved := filepath.Join(repo, filepath.Dir(f), pathPart)
			if pathPart == "" {
				resolved = filepath.Join(repo, f)
			}
			if rel, err := filepath.Rel(repo, resolved); err == nil && (rel == "artifacts" || strings.HasPrefix(rel, "artifacts"+string(filepath.Separator))) {
				continue
			}
			st, err := os.Stat(resolved)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: broken link %s", f, target))
				continue
			}
			if frag == "" || st.IsDir() || !strings.HasSuffix(resolved, ".md") {
				continue
			}
			set, ok := cache[resolved]
			if !ok {
				if set, err = targetAnchors(repo, resolved); err != nil {
					return nil, err
				}
				cache[resolved] = set
			}
			if !set[frag] {
				problems = append(problems, fmt.Sprintf("%s: missing anchor %s", f, target))
			}
		}
	}
	return problems, nil
}
