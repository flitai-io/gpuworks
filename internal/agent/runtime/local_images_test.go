package runtime

import (
	"strings"
	"testing"
)

func TestLocalImageListParsingAndSelection(t *testing.T) {
	const ref = "registry.example:5000/team/runner:v1"
	id := "sha256:" + strings.Repeat("a", 64)
	input := strings.Join([]string{"registry.example:5000/team/runner\tv1\t" + id + "\t4 GiB\r", "<none>\t<none>\t" + id + "\t1 GiB", "-flag\tv1\t" + id + "\t1 GiB", "duplicate\tv1\tinvalid\t2 GiB", "registry.example:5000/team/runner\tv1\t" + id + "\t4 GiB"}, "\n")
	items := ParseLocalImages(input)
	if len(items) != 1 || items[0].Ref != ref || items[0].Size != "4 GiB" || items[0].Identity != id {
		t.Fatalf("parsed images: %+v", items)
	}
	for value, want := range map[string]bool{ref: true, "runner:dev": true, "registry.example:5000/team/runner": false, "-runner:v1": false, "<none>:v1": false, "runner@sha256:abcd": false, "runner:": false} {
		if ValidLocalImageRef(value) != want {
			t.Errorf("ref %q: want %t", value, want)
		}
	}
	for value, want := range map[string]bool{"": true, "k8s.io": true, "team-2": true, "-n": false, "../other": false, "a b": false} {
		if ValidImageNamespace(value) != want {
			t.Errorf("namespace %q: want %t", value, want)
		}
	}
}
