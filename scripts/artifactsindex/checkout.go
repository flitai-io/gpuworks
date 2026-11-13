package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// artifactsDir is where the private artifacts repository is checked out
// inside the main checkout. The main repository ignores it (.gitignore) and
// must never track a file under it: a branch cut before the split that adds
// or edits a file there would otherwise bring it back on merge.
const artifactsDir = "artifacts"

// planFile holds the hand-written roadmap and the generated active-plans
// block. It lives in the artifacts repository so a new or closed plan never
// needs a commit in the main repository.
const planFile = artifactsDir + "/plan.md"

// artifactsCheckedOut reports whether the artifacts repository is present.
// Its README.md is the marker: an empty leftover directory does not count.
func artifactsCheckedOut(root string) bool {
	_, err := os.Stat(filepath.Join(root, artifactsDir, "README.md"))
	return err == nil
}

// inArtifacts reports whether a repo-root-relative path is inside artifacts/.
func inArtifacts(relPath string) bool {
	return strings.HasPrefix(relPath, artifactsDir+"/")
}

// trackedUnderArtifacts lists the files the main repository's index holds
// under artifacts/. Outside a git work tree (the package's own fixtures) it
// reports nothing.
func trackedUnderArtifacts(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil, nil //nolint:nilerr // Fixture directories outside a Git checkout have no tracked artifacts.
	}
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--", artifactsDir+"/")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files, nil
}
