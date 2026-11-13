package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseDocsArtifactRetention(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".gitlab-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var release struct {
		Artifacts struct {
			ExpireIn string `yaml:"expire_in"`
		}
		Rules  []struct{ If, When string }
		Script []string
	}
	// MR pipelines retain lightweight checks without installing a browser or
	// printing full handbooks. Only formal version tags produce PDF artifacts.
	var nodes map[string]yaml.Node
	if err := yaml.Unmarshal(data, &nodes); err != nil {
		t.Fatal(err)
	}
	if _, exists := nodes["docs-pdf-preview"]; exists {
		t.Fatal("MR pipelines must not generate PDF previews by default")
	}
	node := nodes["release-docs-pdf"]
	if err := node.Decode(&release); err != nil {
		t.Fatal(err)
	}
	if release.Artifacts.ExpireIn != "never" {
		t.Fatalf("release retention = %q, want never", release.Artifacts.ExpireIn)
	}
	if len(release.Rules) != 2 || release.Rules[0].If != `$CI_COMMIT_TAG =~ /^v[0-9]+\.[0-9]+\.[0-9]+$/` || release.Rules[1].When != "never" {
		t.Fatalf("permanent artifacts must only run for formal tags: %+v", release.Rules)
	}
	if !strings.Contains(strings.Join(release.Script, "\n"), `--version "$CI_COMMIT_TAG"`) {
		t.Fatal("release PDFs must use the formal version tag")
	}
}

// The GitHub release workflow keeps the same contract, and never publishes to
// the distribution sources unless the repository switches it on: until then
// GitLab is the only publisher of every channel.
func TestReleaseWorkflowContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			If          string
			Needs       []string
			Environment string
			Concurrency struct {
				Group            string
				CancelInProgress bool `yaml:"cancel-in-progress"`
			}
			Steps []struct {
				Run  string
				With map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if got := workflow.Jobs["docs-pdf"].If; got != "github.ref_type == 'tag'" {
		t.Fatalf("docs-pdf if = %q: permanent handbooks must only print for version tags", got)
	}
	pdf := ""
	for _, step := range workflow.Jobs["docs-pdf"].Steps {
		pdf += step.Run
	}
	if !strings.Contains(pdf, `--version "$VERSION"`) {
		t.Fatal("release PDFs must use the formal version tag")
	}
	publish := workflow.Jobs["publish"]
	if !strings.Contains(publish.If, "vars.GPUWORKS_DIST_PUBLISH == 'true'") {
		t.Fatalf("publish if = %q: publishing must stay behind the GPUWORKS_DIST_PUBLISH switch", publish.If)
	}
	if !strings.Contains(publish.If, "needs.build.result == 'success'") {
		t.Fatalf("publish if = %q: every build must succeed before a publish", publish.If)
	}
	if publish.Environment != "distribution" {
		t.Fatalf("publish environment = %q, want distribution (its secrets and ref rules)", publish.Environment)
	}
	if publish.Concurrency.Group != "distribution" || publish.Concurrency.CancelInProgress {
		t.Fatalf("publish concurrency = %+v: publishes must queue, never cancel one another", publish.Concurrency)
	}
	script := ""
	for _, step := range publish.Steps {
		script += step.Run
	}
	if !strings.Contains(script, `./scripts/publish_dist.sh "$CHANNEL" dist`) {
		t.Fatal("publish must go through scripts/publish_dist.sh")
	}
}
