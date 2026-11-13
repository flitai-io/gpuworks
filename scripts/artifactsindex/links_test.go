package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindLinkIssuesFlagsDanglingReference(t *testing.T) {
	root := t.TempDir()
	issues := findLinkIssues(root, "doc.md", "see `artifacts/work/missing.md` for detail\n")
	if len(issues) != 1 {
		t.Fatalf("expected exactly one issue, got %v", issues)
	}
	if issues[0].Ref != "artifacts/work/missing.md" {
		t.Fatalf("unexpected ref: %q", issues[0].Ref)
	}
	if issues[0].Line != 1 {
		t.Fatalf("expected line 1, got %d", issues[0].Line)
	}
}

func TestFindLinkIssuesAllowsExistingFileOrDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "artifacts/work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts/work/real.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := "a file `artifacts/work/real.md` and a directory `artifacts/work/`\n"
	if issues := findLinkIssues(root, "doc.md", text); len(issues) != 0 {
		t.Fatalf("expected no issues, got %v", issues)
	}
}

func TestFindLinkIssuesSkipsPlaceholderTemplates(t *testing.T) {
	root := t.TempDir()
	text := "新计划落 `artifacts/work/<YYYY-MM-DD>-<主题>/plan.md`，见其中说明\n"
	if issues := findLinkIssues(root, "doc.md", text); len(issues) != 0 {
		t.Fatalf("expected the <占位符> template to be skipped, got %v", issues)
	}
}

func TestFindLinkIssuesSkipsGlobPatterns(t *testing.T) {
	root := t.TempDir()
	text := "见 `artifacts/archive/2026-09-22-*-2026-09-22.md`\n"
	if issues := findLinkIssues(root, "doc.md", text); len(issues) != 0 {
		t.Fatalf("expected the glob pattern to be skipped, got %v", issues)
	}
}

func TestFindLinkIssuesSkipsPathFragmentsFromOtherWorkspaces(t *testing.T) {
	root := t.TempDir()
	// "artifacts/..." here is a suffix of an absolute path on a different
	// machine/worktree, not a claim about this repository's tree.
	text := "见 mirror1 `~/workspace/other_repo/artifacts/research/x/`\n"
	if issues := findLinkIssues(root, "doc.md", text); len(issues) != 0 {
		t.Fatalf("expected the foreign-path fragment to be skipped, got %v", issues)
	}
}

func TestFindLinkIssuesSkipsGitRefQualifiedPaths(t *testing.T) {
	root := t.TempDir()
	text := "见 `some-branch:artifacts/research/x/README.md`（该分支上的路径）\n"
	if issues := findLinkIssues(root, "doc.md", text); len(issues) != 0 {
		t.Fatalf("expected the branch:path git ref to be skipped, got %v", issues)
	}
}

func TestFindLinkIssuesSkipsGitRefQualifiedPathsWithSlashInRefName(t *testing.T) {
	root := t.TempDir()
	// A remote-qualified branch name ("origin/develop-weiw") still ends in
	// ':' right before "artifacts/", which is what actually marks this as
	// git object syntax rather than a workspace path.
	text := "用 `git show origin/develop-weiw:artifacts/research/x/README.md` 查看\n"
	if issues := findLinkIssues(root, "doc.md", text); len(issues) != 0 {
		t.Fatalf("expected the origin/<branch>:path git ref to be skipped, got %v", issues)
	}
}

func TestFindLinkIssuesSkipsGitRefQualifiedPathsWithBareBranchName(t *testing.T) {
	root := t.TempDir()
	text := "见 `develop-weiw:artifacts/research/x/README.md`\n"
	if issues := findLinkIssues(root, "doc.md", text); len(issues) != 0 {
		t.Fatalf("expected the develop-weiw:path git ref to be skipped, got %v", issues)
	}
}

func TestIsGitRefColonOnlyMatchesColon(t *testing.T) {
	if !isGitRefColon(':') {
		t.Error("expected ':' to be recognized as a git-ref separator")
	}
	for _, r := range []byte{'/', '.', '-', '_', 'a', '0'} {
		if isGitRefColon(r) {
			t.Errorf("did not expect %q to be treated as a git-ref separator", r)
		}
	}
}

func TestFindLinkIssuesReportsMultipleLinesWithCorrectLineNumbers(t *testing.T) {
	root := t.TempDir()
	text := "line one\nsee `artifacts/work/missing-a.md`\nline three\nsee `artifacts/work/missing-b.md`\n"
	issues := findLinkIssues(root, "doc.md", text)
	if len(issues) != 2 {
		t.Fatalf("expected two issues, got %v", issues)
	}
	if issues[0].Line != 2 || issues[1].Line != 4 {
		t.Fatalf("unexpected line numbers: %+v", issues)
	}
}

// --- scope: which files checkLinks actually reads ---------------------

func TestCheckLinksIgnoresDanglingLinkInsideAnArchivedDoc(t *testing.T) {
	root := t.TempDir()
	// A closed archive/ topic's own dangling link is closed history, never
	// scanned, regardless of whether the file happens to carry frontmatter.
	mkfile(t, root, "artifacts/archive/2026-09-01-x/plan.md",
		fm("done")+"\n见 `artifacts/work/does-not-exist.md`\n")

	issues, err := checkLinks(root, []string{"artifacts/archive/2026-09-01-x/plan.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("an archive/ doc's own dangling link must not be scanned, got %v", issues)
	}
}

func TestCheckLinksIgnoresDanglingLinkInsideAResearchDoc(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "artifacts/research/2026-09-01-x/README.md",
		fm("done")+"\n见 `artifacts/work/does-not-exist.md`\n")

	issues, err := checkLinks(root, []string{"artifacts/research/2026-09-01-x/README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("a research/ doc's own dangling link must not be scanned, got %v", issues)
	}
}

func TestCheckLinksFlagsDanglingLinkInsideAWorkDoc(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "artifacts/work/2026-09-01-x/plan.md",
		fm("proposed")+"\n见 `artifacts/work/does-not-exist.md`\n")

	issues, err := checkLinks(root, []string{"artifacts/work/2026-09-01-x/plan.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Ref != "artifacts/work/does-not-exist.md" {
		t.Fatalf("expected the work/ doc's dangling link to be flagged, got %v", issues)
	}
}

func TestCheckLinksFlagsDanglingLinkInsideAWorkFileWithoutFrontmatter(t *testing.T) {
	root := t.TempDir()
	// Every .md file under artifacts/work/ is scanned, whether or not it
	// carries frontmatter — a plain evidence file dropped next to an
	// in-progress plan is still live, current work.
	mkfile(t, root, "artifacts/work/2026-09-01-x/notes.md", "见 `artifacts/work/does-not-exist.md`\n")

	issues, err := checkLinks(root, []string{"artifacts/work/2026-09-01-x/notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected the frontmatter-less work/ file's dangling link to be flagged, got %v", issues)
	}
}

func TestCheckLinksScansCodeAndDocsTreeButNotArtifacts(t *testing.T) {
	root := t.TempDir()
	// docs/ is always in scope.
	mkfile(t, root, "docs/note.md", "见 `artifacts/work/missing.md`\n")
	// A .go file anywhere is in scope.
	mkfile(t, root, "internal/foo/bar.go", "// see artifacts/work/missing2.md\n")
	// An arbitrary evidence file under artifacts/archive/ (no frontmatter,
	// not docs/, not code) is never in scope.
	mkfile(t, root, "artifacts/archive/2026-09-01-x/some-old-report.md", "见 `artifacts/work/missing3.md`\n")

	issues, err := checkLinks(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, iss := range issues {
		got[iss.Ref] = true
	}
	if !got["artifacts/work/missing.md"] {
		t.Errorf("expected docs/note.md's dangling link to be flagged: %v", issues)
	}
	if !got["artifacts/work/missing2.md"] {
		t.Errorf("expected the .go file's dangling link to be flagged: %v", issues)
	}
	if got["artifacts/work/missing3.md"] {
		t.Errorf("an archive/ evidence file must not be scanned: %v", issues)
	}
}

func TestCheckLinksExcludesTheHistoryFilterPathsManifest(t *testing.T) {
	root := t.TempDir()
	// Every line here is intentionally a removed, non-existent path (that is
	// the whole point of the file); it must never be scanned.
	mkfile(t, root, "scripts/history-filter-paths.txt", "artifacts/acceptance/does-not-exist/screenshot.png\n")

	issues, err := checkLinks(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("expected scripts/history-filter-paths.txt to be excluded entirely, got %v", issues)
	}
}

func TestIsSelfTestFixtureExemptsOnlyThisPackagesTests(t *testing.T) {
	if !isSelfTestFixture("scripts/artifactsindex/links_test.go") {
		t.Error("expected this package's own _test.go to be exempt")
	}
	if isSelfTestFixture("internal/server/api/models_test.go") {
		t.Error("did not expect an unrelated package's test file to be exempt")
	}
}

func TestIsCodeOrConfigCoversBothCIDefinitions(t *testing.T) {
	for _, path := range []string{".gitlab-ci.yml", ".github/workflows/ci.yml", ".github/workflows/release.yml", "Makefile"} {
		if !isCodeOrConfig(path) {
			t.Errorf("isCodeOrConfig(%q) = false, want true", path)
		}
	}
	for _, path := range []string{".github/PULL_REQUEST_TEMPLATE.md", "docs/README.md"} {
		if isCodeOrConfig(path) {
			t.Errorf("isCodeOrConfig(%q) = true, want false", path)
		}
	}
}
