package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationRetainsOnlyVerifiedChannelHistory(t *testing.T) {
	var prior string
	for _, id := range []string{"aaaa", "bbbb", "cccc", "cccc"} {
		in := realInput(t, distDir(t, "gpuworks-"+id+"-linux-amd64.tar.gz"))
		in.Keep, in.Previous = 2, prior
		index, err := assemble(in)
		if err != nil {
			t.Fatal(err)
		}
		if id == "cccc" && (len(index.Servers()) != 2 || index.PreviousServers[0].ReleaseID != "bbbb") {
			t.Fatalf("retained %v", index.Servers())
		}
		prior = filepath.Join(in.OutDir, "nightly", "index.json")
	}
	in := realInput(t, distDir(t, "gpuworks-dddd-linux-amd64.tar.gz"))
	in.Previous, in.Channel = prior, "release"
	if _, err := assemble(in); err == nil {
		t.Fatal("nightly history must never be advertised as release")
	}
	if err := os.WriteFile(prior, []byte("unsigned"), 0600); err != nil {
		t.Fatal(err)
	}
	in.Channel = "nightly"
	in.OutDir = filepath.Join(t.TempDir(), "tree")
	if _, err := assemble(in); err == nil {
		t.Fatal("refuse unverified history")
	}
}
