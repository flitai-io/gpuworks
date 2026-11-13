// Command changerecord fails a diff that changes a contract or decision
// path without the matching record. The path list is scripts/change-record-rules.txt.
// Contract records live in docs/ of the same diff. Decision records live in a
// same-named branch of gpuworks-artifacts, in a plan.md frontmatter. This
// command never writes an artifacts path into product docs.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "change-record: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Companion MRs may already be merged when the default-branch or release
	// pipeline starts. Enforce this gate before merging, not after it.
	if os.Getenv("CI") != "" && os.Getenv("CI_PIPELINE_SOURCE") != "merge_request_event" {
		fmt.Println("change-record: non-MR CI pipeline; checked by the merge-request gate")
		return nil
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	rulesText, err := os.ReadFile(filepath.Join(root, "scripts", "change-record-rules.txt"))
	if err != nil {
		return err
	}
	rules, err := parseRules(string(rulesText))
	if err != nil {
		return err
	}
	base, err := diffBase(root)
	if err != nil {
		if os.Getenv("CI_PIPELINE_SOURCE") == "merge_request_event" {
			return err
		}
		fmt.Printf("NOTE: change-record: %v; skipped\n", err)
		return nil
	}
	files, err := diffNames(root, base)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Println("change-record: no diff")
		return nil
	}
	contract, decision := classify(rules, files)
	var problems []string
	if len(contract) > 0 {
		records, err := diffNames(root, base, "--diff-filter=AM", "--", "docs/")
		if err != nil {
			return err
		}
		if !contractSatisfied(records) {
			problems = append(problems, fmt.Sprintf("contract paths changed without added/updated docs/ or docs/adr/:\n  %s", strings.Join(contract, "\n  ")))
		}
	}
	if len(decision) > 0 {
		branch := sourceBranch()
		bodies, lookErr := decisionPlans(root, branch)
		if lookErr != nil {
			problems = append(problems, fmt.Sprintf("decision paths changed, record not verified (%v):\n  %s", lookErr, strings.Join(decision, "\n  ")))
		} else if !decisionRecorded(bodies) {
			problems = append(problems, fmt.Sprintf("decision paths changed, but no plan.md on branch %s has decision and rejected:\n  %s", branch, strings.Join(decision, "\n  ")))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	fmt.Printf("change-record: ok (%d files, contract %d, decision %d)\n", len(files), len(contract), len(decision))
	return nil
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not a git checkout")
	}
	return strings.TrimSpace(string(out)), nil
}

func diffBase(root string) (string, error) {
	if base := os.Getenv("CI_MERGE_REQUEST_DIFF_BASE_SHA"); base != "" {
		return base, nil
	}
	if base := os.Getenv("CHANGE_RECORD_BASE"); base != "" {
		return base, nil
	}
	for _, ref := range []string{"wei/master", "origin/master"} {
		if exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", ref).Run() != nil {
			continue
		}
		out, err := exec.Command("git", "-C", root, "merge-base", "HEAD", ref).Output()
		if err != nil {
			continue
		}
		return strings.TrimSpace(string(out)), nil
	}
	return "", fmt.Errorf("no merge base")
}

func diffNames(root, base string, filters ...string) ([]string, error) {
	cmd := exec.Command("git", append([]string{"-C", root, "diff", "--no-renames", "--name-only", "-z", base + "...HEAD"}, filters...)...)
	out, err := cmd.Output()
	if err != nil {
		cmd = exec.Command("git", append([]string{"-C", root, "diff", "--no-renames", "--name-only", "-z", base, "HEAD"}, filters...)...)
		out, err = cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git diff: %w", err)
		}
	}
	var files []string
	for _, line := range strings.Split(string(out), "\x00") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func sourceBranch() string {
	if b := os.Getenv("CI_MERGE_REQUEST_SOURCE_BRANCH_NAME"); b != "" {
		return b
	}
	if b := os.Getenv("CHANGE_RECORD_BRANCH"); b != "" {
		return b
	}
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	b := strings.TrimSpace(string(out))
	if b == "HEAD" {
		return ""
	}
	return b
}

func decisionPlans(root, branch string) ([]string, error) {
	if branch == "" || branch == "master" {
		return nil, fmt.Errorf("no feature branch name")
	}
	if os.Getenv("CI_PIPELINE_SOURCE") == "merge_request_event" {
		return gitlabArtifactPlans(branch)
	}
	if bodies, err := localArtifactPlans(root, branch); err == nil {
		return bodies, nil
	}
	return gitlabArtifactPlans(branch)
}

func localArtifactPlans(root, branch string) ([]string, error) {
	dir := filepath.Join(root, "artifacts")
	top, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	realDir, pathErr := filepath.EvalSymlinks(dir)
	if err != nil || pathErr != nil || strings.TrimSpace(string(top)) != realDir {
		return nil, fmt.Errorf("independent artifacts checkout missing")
	}
	if exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", branch).Run() != nil {
		return nil, fmt.Errorf("artifacts branch %s missing", branch)
	}
	var base string
	for _, ref := range []string{"origin/master", "wei/master", "master"} {
		out, err := exec.Command("git", "-C", dir, "merge-base", branch, ref).Output()
		if err == nil {
			base = strings.TrimSpace(string(out))
			break
		}
	}
	if base == "" {
		return nil, fmt.Errorf("no artifacts merge base for %s", branch)
	}
	out, err := exec.Command("git", "-C", dir, "diff", "--name-only", "--diff-filter=AM", "-z", base+"..."+branch).Output()
	if err != nil {
		return nil, err
	}
	var bodies []string
	for _, name := range strings.Split(string(out), "\x00") {
		if filepath.Base(name) != "plan.md" {
			continue
		}
		raw, err := exec.Command("git", "-C", dir, "show", branch+":"+name).Output()
		if err != nil {
			continue
		}
		bodies = append(bodies, string(raw))
	}
	if len(bodies) == 0 {
		return nil, fmt.Errorf("no plan.md on artifacts branch %s", branch)
	}
	return bodies, nil
}

func gitlabArtifactPlans(branch string) ([]string, error) {
	token := os.Getenv("CHANGE_RECORD_ARTIFACTS_TOKEN")
	if token == "" {
		token = os.Getenv("GITLAB_TOKEN")
	}
	if token == "" {
		return nil, fmt.Errorf("set CHANGE_RECORD_ARTIFACTS_TOKEN with read_api access to the artifacts project (GitLab 14 CI_JOB_TOKEN does not support MR changes)")
	}
	api := os.Getenv("CI_API_V4_URL")
	if api == "" {
		api = "http://dev.zstack.io:9080/api/v4"
	}
	project := os.Getenv("CHANGE_RECORD_ARTIFACTS_PROJECT")
	if project == "" {
		project = "wei.wang/gpuworks-artifacts"
	}
	client := &http.Client{Timeout: 20 * time.Second}
	mrs, err := gitlabGet(client, token, api+"/projects/"+url.PathEscape(project)+"/merge_requests?source_branch="+url.QueryEscape(branch)+"&state=opened")
	if err != nil {
		return nil, err
	}
	var list []struct {
		IID int `json:"iid"`
	}
	if err := json.Unmarshal(mrs, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no open gpuworks-artifacts MR for branch %s", branch)
	}
	var bodies []string
	for _, mr := range list {
		changes, err := gitlabGet(client, token, fmt.Sprintf("%s/projects/%s/merge_requests/%d/changes", api, url.PathEscape(project), mr.IID))
		if err != nil {
			return nil, err
		}
		var payload struct {
			SourceProjectID int `json:"source_project_id"`
			DiffRefs        struct {
				HeadSHA string `json:"head_sha"`
			} `json:"diff_refs"`
			Overflow bool `json:"overflow"`
			Changes  []struct {
				NewPath     string `json:"new_path"`
				DeletedFile bool   `json:"deleted_file"`
				Diff        string `json:"diff"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(changes, &payload); err != nil {
			return nil, err
		}
		if payload.Overflow {
			return nil, fmt.Errorf("artifacts MR %d diff is truncated; record not verified", mr.IID)
		}
		if payload.DiffRefs.HeadSHA == "" {
			return nil, fmt.Errorf("artifacts MR %d has no verified head revision", mr.IID)
		}
		sourceProject := project
		if payload.SourceProjectID > 0 {
			sourceProject = fmt.Sprint(payload.SourceProjectID)
		}
		for _, ch := range payload.Changes {
			if ch.DeletedFile || filepath.Base(ch.NewPath) != "plan.md" {
				continue
			}
			raw, err := gitlabGet(client, token, fmt.Sprintf("%s/projects/%s/repository/files/%s/raw?ref=%s", api, url.PathEscape(sourceProject), url.PathEscape(ch.NewPath), url.QueryEscape(payload.DiffRefs.HeadSHA)))
			if err != nil {
				return nil, err
			}
			bodies = append(bodies, string(raw))
		}
	}
	if len(bodies) == 0 {
		return nil, fmt.Errorf("artifacts MR for %s changes no plan.md", branch)
	}
	return bodies, nil
}

func gitlabGet(client *http.Client, token, rawURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gitlab %s; check read_api credential and artifacts project access", resp.Status)
	}
	return body, nil
}
