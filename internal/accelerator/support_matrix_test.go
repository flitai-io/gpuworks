package accelerator

import (
	"os"
	"strings"
	"testing"
)

// The declared-support table of the support matrix is written from the
// vendors' definitions, in both languages. GPUWORKS_UPDATE_SUPPORT_MATRIX=1
// rewrites it; without it, a table that differs from the definitions fails.
func TestSupportMatrixTableIsWrittenFromTheDefinitions(t *testing.T) {
	const begin, end = "<!-- declared-support:begin -->", "<!-- declared-support:end -->"
	for _, page := range []struct {
		path       string
		header     []string
		none, any  string
		sep        string
		open, shut string
	}{
		{"../../docs/reference/support-matrix.md", []string{"厂商", "镜像框架", "概览命令", "要求的宿主机", "可承载负载的运行时", "不提供的平台功能", "dGPU 后端（运行时）"}, "无", "不限", "、", "（", "）"},
		{"../../docs-en/reference/support-matrix.md", []string{"Vendor", "Image framework", "Overview command", "Required host", "Runtimes that carry workloads", "Platform features not offered", "dGPU backend (runtimes)"}, "none", "any", ", ", " (", ")"},
	} {
		raw, err := os.ReadFile(page.path)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(raw)
		from, to := strings.Index(doc, begin), strings.Index(doc, end)
		if from < 0 || to < from {
			t.Fatalf("%s has no %s … %s block", page.path, begin, end)
		}
		want := begin + "\n" + declaredSupportTable(page.header, page.none, page.any, page.sep, page.open, page.shut) + end
		if got := doc[from : to+len(end)]; got != want {
			if os.Getenv("GPUWORKS_UPDATE_SUPPORT_MATRIX") == "" {
				t.Errorf("%s: the declared-support table differs from the vendors' definitions; rerun with GPUWORKS_UPDATE_SUPPORT_MATRIX=1, then record the page's new source hash with python3 docs-site/check_translations.py --write docs/reference/support-matrix.md\n got:\n%s\nwant:\n%s", page.path, got, want)
				continue
			}
			if err := os.WriteFile(page.path, []byte(doc[:from]+want+doc[to+len(end):]), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func declaredSupportTable(header []string, none, any, sep, open, shut string) string {
	code := func(values []string) string {
		if len(values) == 0 {
			return none
		}
		return "`" + strings.Join(values, "`"+sep+"`") + "`"
	}
	var b strings.Builder
	b.WriteString("| " + strings.Join(header, " | ") + " |\n|" + strings.Repeat("---|", len(header)) + "\n")
	for _, v := range Vendors() {
		var missing []string
		for _, feature := range Unsupported(v.ID, Features...) {
			missing = append(missing, string(feature))
		}
		backend := none
		if v.PartitionBackend != "" {
			backend = "`" + v.PartitionBackend + "`" + open + code(v.PartitionRuntimes) + shut
		}
		tool := none
		if v.Tool != "" {
			tool = "`" + v.Tool + "`"
		}
		host := any
		if v.HostOS != "" || v.HostArch != "" {
			host = "`" + strings.Trim(v.HostOS+"/"+v.HostArch, "/") + "`"
		}
		b.WriteString("| " + strings.Join([]string{v.Brand, code(v.Frameworks), tool, host, code(v.Runtimes), code(missing), backend}, " | ") + " |\n")
	}
	return b.String()
}
