package modelscan

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

// Reasons a directory was looked at but is not offered as a model.
const (
	SkipNoWeights          = proto.ScanSkipNoWeights
	SkipEmpty              = proto.ScanSkipEmpty
	SkipMetadataOnly       = proto.ScanSkipMetadataOnly
	SkipHFHubCache         = proto.ScanSkipHFHubCache
	SkipTooDeep            = proto.ScanSkipTooDeep
	SkipUnreadable         = proto.ScanSkipUnreadable
	SkipUnrecognizedLayout = proto.ScanSkipUnrecognizedLayout
)

const (
	defaultMaxDepth = 4
	defaultLimit    = 200
)

// Candidate is one model directory found by Scan.
type Candidate struct {
	RelPath   string // slash-separated, relative to the scan root
	SizeBytes int64  // regular files, excluding dot directories and dot files
	Files     int
	Source    string
	SourceID  string
	Revision  string
	Subfolder string
	Basis     string
	Format    string
	Tasks     []string
	Issues    []modelmetadata.Issue

	Incomplete       bool
	IncompleteReason string
}

// Skipped is a directory that was examined and left out.
type Skipped struct{ RelPath, Reason string }

// Options tunes Scan. Zero values pick the defaults.
type Options struct {
	MaxDepth int       // default 4; the root's children are depth 1
	Limit    int       // max candidates per call, default 200
	Cursor   string    // resume after this RelPath (exclusive); "" starts over
	Deadline time.Time // zero means none
}

// Result is one page of a scan.
type Result struct {
	Candidates []Candidate
	Skipped    []Skipped
	NextCursor string // resume point when Complete is false
	Complete   bool
}

type node struct {
	rel     string
	depth   int
	scope   string
	context modelmetadata.Context
}

type scanner struct {
	root         string
	resolvedRoot string
	maxDepth     int
	cursor       string
	hasCursor    bool
	res          Result
	queue        []node
	limit        int
	full         bool
}

// Scan walks root breadth first (siblings in name order) and returns the model
// entries identified by the metadata parser. Finding a model does not hide
// independent entries below it. Dot directories are never entered, and symlinked directories
// are followed only while they stay inside root.
//
// A call returns at most Options.Limit candidates; resume with NextCursor.
// Cancellation and the deadline end the call early with Complete=false and no
// error; only an unreadable root is an error.
func Scan(ctx context.Context, root string, o Options) (Result, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Result{}, err
	}
	rootEntries, err := os.ReadDir(resolved)
	if err != nil {
		return Result{}, err
	}
	s := &scanner{root: root, resolvedRoot: resolved, maxDepth: o.MaxDepth, limit: o.Limit}
	if s.maxDepth <= 0 {
		s.maxDepth = defaultMaxDepth
	}
	if s.limit <= 0 {
		s.limit = defaultLimit
	}
	if c := strings.Trim(filepath.ToSlash(o.Cursor), "/"); c != "" {
		s.cursor, s.hasCursor = c, true
	}
	ls := s.classifyEntries(root, rootEntries)
	for _, name := range ls.dirs {
		s.queue = append(s.queue, node{rel: name, depth: 1})
	}

	interrupted := false
	for len(s.queue) > 0 && !s.full {
		if ctx.Err() != nil || (!o.Deadline.IsZero() && !time.Now().Before(o.Deadline)) {
			interrupted = true
			break
		}
		n := s.queue[0]
		s.queue = s.queue[1:]
		s.visit(ctx, n)
	}

	if len(s.res.Candidates) > 0 {
		s.res.NextCursor = s.res.Candidates[len(s.res.Candidates)-1].RelPath
	} else if interrupted {
		// Nothing new this call: keep the caller's resume point.
		s.res.NextCursor = s.cursor
	}
	if interrupted {
		// Skips after the resume point will be seen again on the next call.
		kept := s.res.Skipped[:0]
		for _, sk := range s.res.Skipped {
			if s.res.NextCursor != "" && proto.ScanOrder(sk.RelPath, s.res.NextCursor) <= 0 {
				kept = append(kept, sk)
			}
		}
		s.res.Skipped = kept
	}
	s.res.Complete = !interrupted && !s.full
	if s.res.Complete {
		s.res.NextCursor = ""
	}
	return s.res, nil
}

// dirListing is a directory's non-dot content.
type dirListing struct {
	dirs  []string // usable subdirectory names, in name order
	files []string // regular files (and symlinks to them)
}

func (s *scanner) classifyEntries(dir string, entries []os.DirEntry) dirListing {
	var ls dirListing
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		switch {
		case e.IsDir():
			ls.dirs = append(ls.dirs, name)
		case e.Type().IsRegular():
			ls.files = append(ls.files, name)
		case e.Type()&fs.ModeSymlink != 0:
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.Mode().IsRegular() && s.insideRoot(path) {
				ls.files = append(ls.files, name)
			} else if info.IsDir() && s.insideRoot(path) {
				ls.dirs = append(ls.dirs, name)
			}
		}
	}
	return ls
}

func (s *scanner) insideRoot(path string) bool {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(s.resolvedRoot, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *scanner) skip(rel, reason string) {
	s.res.Skipped = append(s.res.Skipped, Skipped{RelPath: rel, Reason: reason})
}

// visit classifies one directory. Directories at or before the cursor were
// already reported by an earlier page: they are classified (so the traversal
// stays identical) but nothing about them is reported.
func (s *scanner) visit(ctx context.Context, n node) {
	done := s.hasCursor && proto.ScanOrder(n.rel, s.cursor) <= 0
	dir := filepath.Join(s.root, filepath.FromSlash(n.rel))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !done {
			s.skip(n.rel, SkipUnreadable)
		}
		return
	}
	ls := s.classifyEntries(dir, entries)

	report, err := s.parseNode(ctx, n)
	if err != nil {
		if !done {
			s.skip(n.rel, SkipUnreadable)
		}
		return
	}
	if report.Prune {
		if !done {
			s.skip(n.rel, SkipHFHubCache)
		}
		return
	}
	if !done {
		switch {
		case len(report.Entries) == 1:
			s.addCandidate(dir, n, report)
		case len(report.Entries) > 1:
			s.skip(n.rel, proto.ScanSkipMultipleEntries)
		case report.Component:
			s.skip(n.rel, proto.ScanSkipComponent)
		case slices.ContainsFunc(report.Issues, func(i modelmetadata.Issue) bool { return i.Code == "metadata_read_limit" }):
			s.skip(n.rel, proto.ScanSkipMetadataLimit)
		case report.HasMetadataIssues():
			if summarizeModelFiles(dir).size == 0 {
				s.skip(n.rel, SkipEmpty)
			} else {
				s.skip(n.rel, proto.ScanSkipInvalidMetadata)
			}
		}
	}
	// Context is reconstructed even for earlier pages. Its paths are relative
	// to scope, never to the storage root or a guessed directory depth.
	n.inheritMetadata(report)
	if len(ls.dirs) > 0 {
		if n.depth >= s.maxDepth {
			if !done {
				s.skip(n.rel, SkipTooDeep)
			}
			return
		}
		for _, name := range ls.dirs {
			s.queue = append(s.queue, node{rel: n.rel + "/" + name, depth: n.depth + 1, scope: n.scope, context: n.context})
		}
		return
	}
	if done || len(report.Entries) > 0 || report.Component || report.HasMetadataIssues() {
		return
	}
	switch {
	case len(ls.files) == 0:
		s.skip(n.rel, SkipEmpty)
	case allMetadata(ls.files):
		s.skip(n.rel, SkipMetadataOnly)
	default:
		s.skip(n.rel, SkipUnrecognizedLayout)
	}
}

func (s *scanner) addCandidate(dir string, n node, report modelmetadata.Report) {
	rel := n.rel
	in := summarizeModelFiles(dir)
	if in.unreadable {
		// Part of the model cannot be listed: its size and completeness are
		// unknown, so it is not offered as a model.
		s.skip(rel, SkipUnreadable)
		return
	}
	if in.size == 0 {
		// Files exist but hold no bytes: an unfinished or wiped download.
		s.skip(rel, SkipEmpty)
		return
	}
	c := Candidate{RelPath: rel, SizeBytes: in.size, Files: in.files}
	identify(dir, rel, &c)
	// A discovered subtree is registered by its physical directory. A remote
	// artifact subfolder preserves repository-relative paths, so inheriting the
	// parent's remote identity would bind a different on-disk layout.
	if n.scope != "" && n.scope != n.rel {
		c.Source, c.SourceID, c.Revision, c.Subfolder, c.Basis = SourceLocal, rel, "", "", BasisPath
	}

	e := report.Entries[0]
	c.Format, c.Tasks, c.Issues = e.Format, e.Tasks, report.Issues
	if e.Incomplete {
		c.Incomplete = true
		c.IncompleteReason = "model metadata could not be verified"
		if len(e.Missing) > 0 {
			c.IncompleteReason = "missing or unresolved files: " + strings.Join(e.Missing, ", ")
		}
	}
	if reason := in.reason(); reason != "" {
		c.Incomplete = true
		if c.IncompleteReason != "" {
			c.IncompleteReason += "; "
		}
		c.IncompleteReason += reason
	}
	s.res.Candidates = append(s.res.Candidates, c)
	if len(s.res.Candidates) >= s.limit {
		s.full = true
	}
}

func (s *scanner) parseNode(ctx context.Context, n node) (modelmetadata.Report, error) {
	scope := n.scope
	if scope == "" {
		scope = n.rel
	}
	v, err := OpenView(filepath.Join(s.root, filepath.FromSlash(scope)))
	if err != nil {
		return modelmetadata.Report{}, err
	}
	defer v.Close()
	name := "."
	if scope != n.rel {
		name = strings.TrimPrefix(n.rel, scope+"/")
	}
	return modelmetadata.Parse(ctx, modelmetadata.Input{Path: name, Context: n.context}, v)
}

// allMetadata reports whether every file is documentation or repo bookkeeping.
func allMetadata(files []string) bool {
	for _, f := range files {
		l := strings.ToLower(f)
		switch {
		case strings.HasSuffix(l, ".md"),
			strings.HasPrefix(l, "license"), strings.HasPrefix(l, "licence"),
			strings.HasPrefix(l, "notice"), strings.HasPrefix(l, "copying"),
			strings.HasPrefix(l, "citation"):
		default:
			return false
		}
	}
	return true
}
