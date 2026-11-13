package modelscan

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

const (
	maxMarkerBytes = 8 << 20
	maxMetaBytes   = 4 << 10
	// maxMetadataProbe bounds how many *.metadata files are opened while
	// looking for a revision.
	maxMetadataProbe = 50
)

// Identity bases, strongest first.
const (
	BasisMarker      = "marker"
	BasisModelScope  = "modelscope"
	BasisHuggingFace = "huggingface"
	BasisPath        = "path"
)

// Model sources.
const (
	SourceModelScope  = "modelscope"
	SourceHuggingFace = "huggingface"
	SourceLocal       = "local"
)

// completionMarker mirrors the fields of the marker JSON that identify a model.
type completionMarker struct {
	Source    string `json:"source"`
	SourceID  string `json:"source_id"`
	Revision  string `json:"revision"`
	Subfolder string `json:"subfolder"`
}

// identify fills Basis/Source/SourceID/Revision/Subfolder for the model at dir.
func identify(dir, rel string, c *Candidate) {
	if m, ok := readMarker(dir); ok {
		c.Basis, c.Source, c.SourceID, c.Revision, c.Subfolder = BasisMarker, m.Source, m.SourceID, m.Revision, m.Subfolder
		return
	}
	c.SourceID = rel
	switch {
	case hasSourceMarker(dir):
		c.Basis, c.Source = BasisModelScope, SourceModelScope
		c.Revision = modelScopeRevision(filepath.Join(dir, modelmetadata.ModelScopeVersionMarker))
	case hfDownloadDir(dir) != "":
		c.Basis, c.Source = BasisHuggingFace, SourceHuggingFace
		c.Revision = huggingFaceRevision(hfDownloadDir(dir))
	default:
		c.Basis, c.Source = BasisPath, SourceLocal
	}
}

// hasSourceMarker reports whether dir holds any modelmetadata.SourceMarkers
// name. The set is modelmetadata's because it is the leaf every reader can
// import; internal/agent/app/modelcopy.go skips the same names when copying.
func hasSourceMarker(dir string) bool {
	for _, marker := range modelmetadata.SourceMarkers {
		if exists(filepath.Join(dir, marker)) {
			return true
		}
	}
	return false
}

// readMarker accepts only a marker that parses and names its source and repo;
// anything weaker falls through to the tool metadata.
func readMarker(dir string) (completionMarker, bool) {
	b, ok := readCapped(filepath.Join(dir, CompletionMarker), maxMarkerBytes)
	if !ok {
		return completionMarker{}, false
	}
	var m completionMarker
	if json.Unmarshal(b, &m) != nil || m.Source == "" || m.SourceID == "" {
		return completionMarker{}, false
	}
	return m, true
}

// modelScopeRevision parses "Revision:master,CreatedAt:1756691498".
func modelScopeRevision(mvPath string) string {
	b, ok := readCapped(mvPath, maxMetaBytes)
	if !ok {
		return ""
	}
	s := string(b)
	const key = "Revision:"
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	s = s[i+len(key):]
	if j := strings.IndexAny(s, ",\r\n"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

// hfDownloadDir returns the huggingface download-state directory of a model
// directory, or "" when there is none.
func hfDownloadDir(dir string) string {
	for _, sub := range []string{
		filepath.Join(".huggingface", "download"),
		filepath.Join(".cache", "huggingface", "download"),
	} {
		root, err := os.OpenRoot(dir)
		if err != nil {
			return ""
		}
		fi, err := root.Stat(sub)
		root.Close()
		if err == nil && fi.IsDir() {
			return filepath.Join(dir, sub)
		}
	}
	return ""
}

// huggingFaceRevision is the commit every file of a huggingface download was
// fetched at: the first line of each *.metadata file (line 1 = commit, 2 =
// etag, 3 = timestamp). Files fetched at different commits - a directory
// downloaded in parts over time - name no single commit, and neither does a
// directory whose metadata is too large to read whole: both are "".
func huggingFaceRevision(downloadDir string) string {
	probed := 0
	rev := ""
	mixed := false
	_ = filepath.WalkDir(downloadDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".metadata") {
			return nil //nolint:nilerr // Optional revision discovery tolerates unreadable metadata.
		}
		if probed++; probed > maxMetadataProbe {
			mixed = true
			return filepath.SkipAll
		}
		sha := firstLine(path)
		if !isHex40(sha) {
			return nil
		}
		if rev != "" && !strings.EqualFold(sha, rev) {
			mixed = true
			return filepath.SkipAll
		}
		rev = sha
		return nil
	})
	if mixed {
		return ""
	}
	return rev
}

// HuggingFaceCommit is the commit a model directory downloaded by the
// huggingface tools (huggingface-cli download --local-dir) was fetched at,
// or "" when it was not, or not at one commit.
func HuggingFaceCommit(dir string) string {
	download := hfDownloadDir(dir)
	if download == "" {
		return ""
	}
	return huggingFaceRevision(download)
}

func firstLine(path string) string {
	f, err := os.OpenInRoot(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReaderSize(io.LimitReader(f, 256), 256).ReadString('\n')
	return strings.TrimSpace(line)
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// modelFileSummary is what one stat-only pass over a model directory learns.
type modelFileSummary struct {
	size  int64
	files int
	// Incomplete-download evidence.
	incomplete, part, partState, tempNonEmpty bool
	// unreadable: a directory under the model could not be listed, so size
	// and file count miss whatever it holds.
	unreadable bool
}

func (in modelFileSummary) reason() string {
	var parts []string
	if in.incomplete {
		parts = append(parts, "*.incomplete present")
	}
	if in.part {
		parts = append(parts, "*.part present")
	}
	if in.partState {
		parts = append(parts, "*.part.state present")
	}
	if in.tempNonEmpty {
		parts = append(parts, "non-empty ._____temp")
	}
	return strings.Join(parts, "; ")
}

func (in *modelFileSummary) note(path string) {
	name := filepath.Base(path)
	switch {
	case strings.HasSuffix(name, ".incomplete"):
		in.incomplete = true
	case strings.HasSuffix(name, ".part.state"):
		in.partState = in.partState || !staleDownloadSidecar(path)
	case strings.HasSuffix(name, ".part"):
		in.part = in.part || !staleDownloadSidecar(path)
	}
}

// summarizeModelFiles sums regular files outside dot entries and looks for unfinished
// downloads. Dot directories are entered only for ".huggingface" and ".cache"
// (download state lives there); they never count towards size or file count.
// It stats; the only contents it reads are a download sidecar's recorded size
// and, when the CLI left *.incomplete files, the checkpoint's shard index.
func summarizeModelFiles(dir string) modelFileSummary {
	var in modelFileSummary
	in.walk(dir, false)
	// The Hugging Face CLI's leftovers name no file; a checkpoint with every
	// shard in place is not waiting on them.
	if in.incomplete && shardsPresent(dir) {
		in.incomplete = false
	}
	return in
}

func (in *modelFileSummary) walk(dir string, inDot bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		in.unreadable = true
		return
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(dir, name)
		switch {
		case e.IsDir():
			switch {
			case name == "._____temp":
				if sub, err := os.ReadDir(path); err == nil && len(sub) > 0 {
					in.tempNonEmpty = true
				}
			case inDot || !strings.HasPrefix(name, "."):
				in.walk(path, inDot)
			case name == ".huggingface" || name == ".cache":
				in.walk(path, true)
			}
		default:
			in.note(path)
			if inDot || strings.HasPrefix(name, ".") {
				continue
			}
			if e.Type().IsRegular() {
				if info, err := e.Info(); err == nil {
					in.size += info.Size()
					in.files++
				}
			} else if e.Type()&fs.ModeSymlink != 0 {
				if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
					in.size += info.Size()
					in.files++
				}
			}
		}
	}
}

// readCapped reads a whole file only if it fits in limit bytes.
func readCapped(path string, limit int64) ([]byte, bool) {
	f, err := os.OpenInRoot(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, false
	}
	return b, true
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
