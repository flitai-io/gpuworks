package diagnostics

import (
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// ServerDir is a directory of the server's whose filesystem a collection
// reports: data (the databases), logs, backups, or sqlite_tmp (where SQLite
// puts its temporary files, see SQLiteTempDir).
type ServerDir struct {
	Kind string
	Path string
}

// serverDiskEntries bounds the files a collection walks to size the
// directories' contents: a full disk is usually one or two large files, and a
// data directory holding millions of small ones is cut short (partial).
const serverDiskEntries = 200_000

// ServerDisk is server/disk.json: how full each of the server's filesystems
// is and what fills its directories, so a disk that ran out can be read from
// the collection instead of inferred from a log that stopped.
type ServerDisk struct {
	Dirs []ServerDirUsage `json:"dirs"`
}

// ServerDirUsage is one directory: its filesystem's room and, except for the
// shared temporary directory, the size of each entry directly inside it,
// largest first.
type ServerDirUsage struct {
	Kind           string           `json:"kind"`
	Path           string           `json:"path"`
	TotalBytes     int64            `json:"total_bytes"`
	AvailableBytes int64            `json:"available_bytes"`
	Error          string           `json:"error,omitempty"`
	Entries        []ServerDirEntry `json:"entries,omitempty"`
	// Partial says the walk stopped at serverDiskEntries files: the entry
	// sizes are lower bounds.
	Partial bool `json:"partial,omitempty"`
}

// ServerDirEntry is a file or directory (Bytes summing what it holds).
type ServerDirEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir,omitempty"`
	Bytes int64  `json:"bytes"`
}

func writeServerDisk(w io.Writer, dirs []ServerDir) (int64, bool, error) {
	report := ServerDisk{Dirs: make([]ServerDirUsage, 0, len(dirs))}
	budget := serverDiskEntries
	for _, d := range dirs {
		usage := ServerDirUsage{Kind: d.Kind, Path: d.Path}
		total, avail, err := FilesystemUsage(d.Path)
		if err != nil {
			usage.Error = err.Error()
		}
		usage.TotalBytes, usage.AvailableBytes = total, avail
		if d.Kind != "sqlite_tmp" && err == nil {
			usage.Entries, usage.Partial = dirEntries(d.Path, &budget)
		}
		report.Dirs = append(report.Dirs, usage)
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return 0, false, err
	}
	n, err := w.Write(raw)
	return int64(n), false, err
}

// dirEntries sizes what is directly inside dir, spending budget on each file
// it visits.
func dirEntries(dir string, budget *int) ([]ServerDirEntry, bool) {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	partial := false
	out := make([]ServerDirEntry, 0, len(list))
	for _, e := range list {
		entry := ServerDirEntry{Name: e.Name(), Dir: e.IsDir()}
		if !e.IsDir() {
			if info, err := e.Info(); err == nil {
				entry.Bytes = info.Size()
			}
			*budget--
		} else {
			_ = filepath.WalkDir(filepath.Join(dir, e.Name()), func(_ string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil //nolint:nilerr // The bounded diagnostic listing skips paths it cannot inspect.
				}
				if *budget <= 0 {
					partial = true
					return filepath.SkipAll
				}
				*budget--
				if d.Type().IsRegular() {
					if info, err := d.Info(); err == nil {
						entry.Bytes += info.Size()
					}
				}
				return nil
			})
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, partial
}

// SQLiteTempDir is the directory SQLite writes its temporary files to on
// Unix: the first of SQLITE_TMPDIR, TMPDIR, /var/tmp, /usr/tmp and /tmp that
// is a directory the process may write and search, else the working
// directory - the order of unixTempFileDir in SQLite's os_unix.c, which the
// server's driver carries over as it is.
func SQLiteTempDir() string {
	for _, dir := range []string{os.Getenv("SQLITE_TMPDIR"), os.Getenv("TMPDIR"), "/var/tmp", "/usr/tmp", "/tmp"} {
		if dir != "" && writableDir(dir) {
			return dir
		}
	}
	return "."
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
