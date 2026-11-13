package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// linkPattern finds candidate "artifacts/..." path references in prose,
// code, and CI YAML. The character class intentionally excludes glob/markup
// punctuation (*, <, >, {, }, backtick, closing brackets, Chinese punctuation
// is non-ASCII and never matches) so those characters naturally end a match
// instead of being swallowed into a bogus path.
var linkPattern = regexp.MustCompile(`artifacts/[A-Za-z0-9_./-]+`)

// linkScanExcludeDirs are pruned during the walk. node_modules and vendor are
// third-party trees; ui/dist and webui/dist are build output that embeds
// copies of source files (and would otherwise double-report every finding).
var linkScanExcludeDirs = map[string]bool{
	"node_modules": true,
	"dist":         true,
	"vendor":       true,
	".git":         true,
}

// fixedNamedFiles are root-level documents checked regardless of extension
// filtering, by exact repo-root-relative name.
var fixedNamedFiles = map[string]bool{
	"CLAUDE.md":       true,
	"CONTEXT.md":      true,
	"README.md":       true,
	"known-issues.md": true,
	planFile:          true,
	"todo.md":         true,
}

// excludedFiles are repo-root-relative paths that are always out of scope,
// no matter which category above would otherwise include them.
var excludedFiles = map[string]bool{
	// scripts/history-filter-paths.txt is a manifest of paths deliberately
	// removed from the working tree, kept only as the argument list for a
	// future `git filter-repo --paths-from-file` history rewrite (see its
	// own header comment and artifacts/archive/2026-09-19-deployment-unification/round1-archive-2026-09/README.md,
	// which says outright that links to these paths are expected to be
	// dangling). Its entries are historical by design, not documentation
	// that should resolve in the working tree.
	"scripts/history-filter-paths.txt": true,
}

// selfTestFixtures are this package's own *_test.go files. They deliberately
// use synthetic fixture paths under a fake "artifacts" tree (see fm and
// mkfile in testrepo_test.go) to exercise findLinkIssues/checkLinks in
// isolation; those strings are test inputs, not documentation claims about
// this repo's tree, so they are exempt the same way check_architecture.sh
// exempts a handful of named false positives with a comment explaining why.
func isSelfTestFixture(relPath string) bool {
	return strings.HasPrefix(relPath, "scripts/artifactsindex/") && strings.HasSuffix(relPath, "_test.go")
}

// isCodeOrConfig reports whether relPath is "production/test code and
// config" per the link-check scope: any .go/.ts/.tsx source file, the CI
// definitions (.gitlab-ci.yml and the GitHub workflows), a Makefile, or
// anything under scripts/ (shell/python/etc. helper scripts, which have no
// single shared extension).
func isCodeOrConfig(relPath string) bool {
	base := filepath.Base(relPath)
	switch {
	case strings.HasSuffix(base, ".go"), strings.HasSuffix(base, ".ts"), strings.HasSuffix(base, ".tsx"):
		return true
	case base == ".gitlab-ci.yml", base == "Makefile":
		return true
	case strings.HasPrefix(relPath, ".github/workflows/"):
		return true
	case relPath == "scripts" || strings.HasPrefix(relPath, "scripts/"):
		return true
	}
	return false
}

// isDocsTree reports whether relPath lives under the repo's docs/ directory.
func isDocsTree(relPath string) bool {
	return relPath == "docs" || strings.HasPrefix(relPath, "docs/")
}

// linkCheckScope decides, file by file, whether checkLinks scans it.
// artifacts/work/ is scanned in full (every .md file there, frontmatter or
// not — it is all live, current work); artifacts/archive/ and
// artifacts/research/ are closed history and are never scanned, whether or
// not a given file happens to carry frontmatter.
type linkCheckScope struct {
	work map[string]bool
}

func newLinkCheckScope(mdFiles []string) linkCheckScope {
	s := linkCheckScope{work: map[string]bool{}}
	for _, f := range mdFiles {
		if strings.HasPrefix(f, "artifacts/work/") {
			s.work[f] = true
		}
	}
	return s
}

func (s linkCheckScope) includes(relPath string) bool {
	if excludedFiles[relPath] {
		return false
	}
	if isSelfTestFixture(relPath) {
		return false
	}
	if isCodeOrConfig(relPath) {
		return true
	}
	if isDocsTree(relPath) {
		return true
	}
	if fixedNamedFiles[relPath] {
		return true
	}
	return s.work[relPath]
}

// isPathContinuation reports whether r can appear inside a larger local or
// absolute path. When the character immediately before a regex match is one
// of these, the match is a suffix of something else (another workspace's
// absolute path, a different git worktree) and is not a claim that this
// literal string is a path from the repo root — skip it rather than
// mis-validate a fragment.
func isPathContinuation(r byte) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.' || r == '/' || r == '-' || r == '_':
		return true
	}
	return false
}

// isGitRefColon reports whether r is the ':' that separates a git ref
// (branch or commit name) from a path in "<ref>:<path>" syntax, e.g.
// "origin/develop-weiw:artifacts/research/x/README.md" or the shorter
// "develop-weiw:artifacts/...". That syntax names an object in git's
// history, not a path in the current working tree, so a match whose
// preceding character is this colon is not a workspace-relative reference
// and must not be checked for on-disk existence.
func isGitRefColon(r byte) bool {
	return r == ':'
}

// knownExampleRefs are exact, individually-justified exceptions for strings
// that read as "artifacts/..." repo paths but are actually illustrative
// command-line examples, never meant to resolve inside this repository.
// Keep this list short and comment every entry; a real dangling reference
// belongs in the referencing document, not in here.
var knownExampleRefs = map[string]bool{
	// artifacts/archive/2026-08-20-images/plan-image-simplification-v1.md's "3.2
	// 重新生成 catalog-seed.json" section shows the -runner-json flag with a
	// concrete-looking path so the example reads naturally; the real input
	// is a local copy of gpustack's upstream runner.py.json release index,
	// fetched by the operator running the command, not a file this repo
	// ships or generates.
	"artifacts/image-catalog/runner.py.json": true,
}

// isGlobOrPlaceholder reports whether the character immediately after a
// match is an obvious continuation the character class deliberately does
// not consume: a glob wildcard, a placeholder's angle/curly bracket. Text
// like "artifacts/work/<YYYY-MM-DD>-<主题>/plan.md" or "artifacts/archive/2026-09-22-*-2026-09-22.md" is
// a template, not a dangling reference, so the truncated match up to that
// point is whitelisted here rather than by listing every such string.
func isGlobOrPlaceholder(r byte) bool {
	switch r {
	case '*', '<', '>', '{', '}':
		return true
	}
	return false
}

// linkIssue is one broken "artifacts/..." reference.
type linkIssue struct {
	File string
	Line int
	Ref  string
}

func (i linkIssue) String() string {
	return fmt.Sprintf("%s:%d: dangling reference %q", i.File, i.Line, i.Ref)
}

// checkLinks walks root and verifies that every "artifacts/..." reference in
// an in-scope file (see linkCheckScope) resolves to a real file or
// directory. mdFiles is every .md file under artifacts/{work,archive,
// research} (scopeFiles' return value), used only to know which paths live
// under artifacts/work/.
func checkLinks(root string, mdFiles []string) ([]linkIssue, error) {
	scope := newLinkCheckScope(mdFiles)
	var issues []linkIssue
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if linkScanExcludeDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !scope.includes(rel) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		issues = append(issues, findLinkIssues(root, rel, string(raw))...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].File != issues[j].File {
			return issues[i].File < issues[j].File
		}
		return issues[i].Line < issues[j].Line
	})
	return issues, nil
}

func findLinkIssues(root, relFile, text string) []linkIssue {
	var out []linkIssue
	for _, m := range linkPattern.FindAllStringIndex(text, -1) {
		start, end := m[0], m[1]
		if start > 0 && isPathContinuation(text[start-1]) {
			continue // fragment of a larger, non-repo-relative reference
		}
		if start > 0 && isGitRefColon(text[start-1]) {
			continue // "<ref>:artifacts/..." git object syntax, not a workspace path
		}
		if end < len(text) && isGlobOrPlaceholder(text[end]) {
			continue // glob or "<placeholder>" template, not a real path
		}
		ref := strings.TrimRight(text[start:end], ".,;:'\"")
		if ref == "" {
			continue
		}
		if knownExampleRefs[ref] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, ref)); err == nil {
			continue // resolves to a real file or directory
		}
		line := 1 + strings.Count(text[:start], "\n")
		out = append(out, linkIssue{File: relFile, Line: line, Ref: ref})
	}
	return out
}
