package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

// lsjsonEntry is one line of `rclone lsjson -R --files-only` output.
type lsjsonEntry struct {
	Path    string    `json:"Path"`
	ModTime time.Time `json:"ModTime"`
}

func pruneCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	var keep int
	var protect stringList
	var nightlyIndex string
	fs.StringVar(&nightlyIndex, "nightly-index", "", "signed nightly index whose listed releases must be retained")
	fs.IntVar(&keep, "keep", 30, "newest nightly (non-version) directories to keep; version tags are always kept")
	fs.Var(&protect, "protect", "release id that must be kept (repeatable): what each channel's latest names")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if nightlyIndex != "" {
		raw, err := os.ReadFile(nightlyIndex)
		if err != nil {
			return err
		}
		payload, _, err := distribution.Open(raw, distribution.TrustedKeys())
		if err != nil {
			return err
		}
		index, err := distribution.ParseIndex(payload, distribution.ChannelNightly)
		if err != nil {
			return err
		}
		for _, release := range index.Servers() {
			protect = append(protect, release.ReleaseID)
		}
	}
	var listing []lsjsonEntry
	if err := json.NewDecoder(stdin).Decode(&listing); err != nil {
		return fmt.Errorf("read rclone lsjson output: %w", err)
	}
	for _, id := range prunable(listing, keep, protect) {
		fmt.Fprintln(stdout, id)
	}
	return nil
}

// prunable returns the release directories to delete. A version tag
// (vYY.M.PATCH) is never deleted: people install and roll back to a named
// release long after it ships. Of the other directories, the nightly builds,
// the newest keep (by their newest file) stay, as do the protected ones.
// Entries are paths relative to releases/, "<id>/<file>".
func prunable(listing []lsjsonEntry, keep int, protect []string) []string {
	newest := map[string]time.Time{}
	for _, entry := range listing {
		id, _, ok := strings.Cut(entry.Path, "/")
		if !ok || id == "" || distribution.IsReleaseVersion(id) {
			continue
		}
		if entry.ModTime.After(newest[id]) {
			newest[id] = entry.ModTime
		}
	}
	ids := make([]string, 0, len(newest))
	for id := range newest {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if !newest[ids[i]].Equal(newest[ids[j]]) {
			return newest[ids[i]].After(newest[ids[j]])
		}
		return ids[i] < ids[j]
	})
	kept := map[string]bool{}
	for _, id := range protect {
		kept[id] = true
	}
	var out []string
	for rank, id := range ids {
		if rank < keep || kept[id] {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
