package proto

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// Storage files: browsing a storage's tree, and uploading, downloading and
// rearranging files in the regions of it that are open to writes (ADR 0053).
// The server picks the node and checks what it can before any byte moves; the
// agent resolves every path against the storage root itself, refuses what
// would leave the root or a writable region, and is the only one touching the
// filesystem.

// CapStorageFiles: the agent serves the storage.files.* RPCs and the
// StreamStorageUpload / StreamStorageDownload streams. The server refuses a
// storage-files request to a node without it rather than sending something an
// older agent drops.
const CapStorageFiles = "storage.files"

// Stream kinds of storage files. Both ride the data lane (LaneFor).
const (
	// StreamStorageUpload carries one file from the server: header StorageID,
	// Rel (target path), Staging, Size (-1 when unknown), Extract, Overwrite,
	// then the bytes. The server half-closes its write side at the end; the
	// agent answers with one StorageFilesStatus line only after the result is
	// in place (or has failed and its staging is gone). A failure may be
	// answered before the body ends (a refusal found before any byte, a bad
	// archive, more bytes than Size); the agent then keeps reading and
	// dropping the body until EOF, so a server may either read the answer
	// while it sends and stop early, or read it after its CloseWrite.
	StreamStorageUpload = "storage.upload"
	// StreamStorageDownload carries one file or one directory to the server:
	// header StorageID, Rel, Format (for a directory). The agent answers first
	// with one StorageDownloadHead line, then, when it has no error, the bytes
	// until EOF: a file as is, a directory as an archive in Format. A failure
	// after the head resets the stream (the server's read fails instead of
	// reaching EOF), which the server reads as a truncated transfer; a
	// refusal in the head is followed by a normal close.
	StreamStorageDownload = "storage.download"
)

// Storage-files RPCs (ctrl lane, short).
const (
	RPCStorageFilesList   = "storage.files.list"
	RPCStorageFilesMkdir  = "storage.files.mkdir"
	RPCStorageFilesRename = "storage.files.rename"
	RPCStorageFilesDelete = "storage.files.delete"
)

// The regions of a storage open to writes, relative to its root. Everything
// else (model directories, the rest of StorageMetaDir) can be browsed and
// downloaded but not changed through storage files.
const (
	// StorageFilesDir is the general-purpose area: whatever a user wants kept
	// on the storage.
	StorageFilesDir = "files"
	// StorageAppsDirName is the apps' data directory inside StorageMetaDir.
	StorageAppsDirName = "apps"
)

// StorageWritableRegions lists the writable regions, as clean slash paths.
func StorageWritableRegions() []string {
	return []string{StorageFilesDir, path.Join(StorageMetaDir, StorageAppsDirName)}
}

// CleanStorageRel normalises a storage-relative path given by a client: slash
// separated, no leading slash, no ".." anywhere, no NUL. An empty path (or "."
// or "/") is the storage root and comes back as "". It does not touch the
// filesystem; the agent still resolves symbolic links before acting.
func CleanStorageRel(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("path contains a NUL byte")
	}
	if strings.Contains(rel, `\`) {
		return "", fmt.Errorf("path %q contains a backslash", rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", fmt.Errorf("path %q contains ..", rel)
		}
	}
	clean := path.Clean("/" + rel)
	return strings.TrimPrefix(clean, "/"), nil
}

// StorageRelWritable reports whether a clean storage-relative path lies
// strictly inside a writable region. The region directories themselves are
// not: they cannot be deleted or renamed, and the agent creates them on the
// first write below them.
func StorageRelWritable(clean string) bool {
	for _, region := range StorageWritableRegions() {
		if strings.HasPrefix(clean, region+"/") && len(clean) > len(region)+1 {
			return true
		}
	}
	return false
}

// StorageUploadStagingRel is where one upload is assembled before it is
// renamed into place: below the control plane's incoming directory, like every
// other write to a storage, so a broken upload never leaves half a file at the
// target. id is unique per upload.
func StorageUploadStagingRel(controlPlaneID, id string) string {
	return StagingRelPath(controlPlaneID, "upload-"+id)
}

// Archive formats: what an upload is unpacked from (StreamHeader.Extract) and
// what a directory download is packed into (StreamHeader.Format).
const (
	StorageArchiveTar   = "tar"
	StorageArchiveTarGz = "tar.gz"
	StorageArchiveZip   = "zip"
)

// StorageArchiveFormatValid reports whether f names an archive format.
func StorageArchiveFormatValid(f string) bool {
	return f == StorageArchiveTar || f == StorageArchiveTarGz || f == StorageArchiveZip
}

// StorageFilesListParams lists one directory of a storage. Path is clean and
// relative to the storage root ("" = the root). Entries come sorted by name
// (byte order); Cursor is the last name of the previous page.
type StorageFilesListParams struct {
	StorageID uint   `json:"storage_id"`
	Path      string `json:"path"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// StorageFilesListMaxLimit bounds one page; the agent clamps Limit to it and
// uses it when Limit is 0.
const StorageFilesListMaxLimit = 500

// StorageFileEntry is one directory entry, read with lstat: a symbolic link is
// reported as one (Symlink, with Dir and Size of the link itself) and never
// followed.
type StorageFileEntry struct {
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	Symlink bool      `json:"symlink,omitempty"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// StorageFilesListResult is one page. NextCursor is empty on the last page.
type StorageFilesListResult struct {
	Entries    []StorageFileEntry `json:"entries"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// StorageFilesMkdirParams creates a directory (and missing parents) inside a
// writable region. An existing directory is not an error.
type StorageFilesMkdirParams struct {
	StorageID uint   `json:"storage_id"`
	Path      string `json:"path"`
}

// StorageFilesRenameParams moves a file or directory within the writable
// regions. The target must not exist.
type StorageFilesRenameParams struct {
	StorageID uint   `json:"storage_id"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// StorageFilesDeleteParams removes a file or directory (recursively) inside a
// writable region: it is renamed into the storage's trash at once and its bytes
// are deleted in the background.
type StorageFilesDeleteParams struct {
	StorageID uint   `json:"storage_id"`
	Path      string `json:"path"`
}

// Storage-files error codes. The agent reports a refusal as a
// StorageFilesError: on an rpc.resp as the code StorageFilesRPCCode names plus
// a "detail" param (its text keeps the "storage_files:<code>: <detail>" shape
// older servers read), and on an upload/download answer in the Code field.
const (
	StorageFilesErrPathInvalid    = "path_invalid"    // not a clean path, or resolves outside the root
	StorageFilesErrRegionReadonly = "region_readonly" // outside the writable regions
	StorageFilesErrNotFound       = "not_found"
	StorageFilesErrExists         = "exists"
	StorageFilesErrNotWritable    = "not_writable"   // the storage is not ready or not writable on this node
	StorageFilesErrExtractFailed  = "extract_failed" // a bad archive, or one the extraction limits refuse
	StorageFilesErrFailed         = "failed"         // anything else
)

const storageFilesErrPrefix = "storage_files:"

// storageFilesRPCCodePrefix namespaces the storage-files codes on an rpc.resp.
const storageFilesRPCCodePrefix = "STORAGE_FILES_"

// StorageFilesError is a storage-files refusal with a stable code.
type StorageFilesError struct {
	Code   string
	Detail string
}

func (e *StorageFilesError) Error() string {
	return storageFilesErrPrefix + e.Code + ": " + e.Detail
}

// RPCCode is the refusal's code on an rpc.resp, e.g. STORAGE_FILES_NOT_FOUND.
func (e *StorageFilesError) RPCCode() string {
	return storageFilesRPCCodePrefix + strings.ToUpper(e.Code)
}

// RPCParams carries the detail on an rpc.resp.
func (e *StorageFilesError) RPCParams() map[string]any {
	return map[string]any{"detail": e.Detail}
}

// NewStorageFilesError builds a refusal; an unknown code becomes failed.
func NewStorageFilesError(code, format string, args ...any) *StorageFilesError {
	switch code {
	case StorageFilesErrPathInvalid, StorageFilesErrRegionReadonly, StorageFilesErrNotFound, StorageFilesErrExists,
		StorageFilesErrNotWritable, StorageFilesErrExtractFailed, StorageFilesErrFailed:
	default:
		code = StorageFilesErrFailed
	}
	return &StorageFilesError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// storageFilesErrorFromRPC rebuilds the refusal an rpc.resp code names; ok is
// false for a code outside the storage-files namespace.
func storageFilesErrorFromRPC(code string, params map[string]any) (*StorageFilesError, bool) {
	name, ok := strings.CutPrefix(code, storageFilesRPCCodePrefix)
	if !ok {
		return nil, false
	}
	detail, _ := params["detail"].(string)
	return NewStorageFilesError(strings.ToLower(name), "%s", detail), true
}

// StorageFilesStatus is the agent's one-line answer at the end of an upload.
type StorageFilesStatus struct {
	State string `json:"state"` // done | failed
	// Code is one of the StorageFilesErr* codes when State is failed.
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
	// Bytes is what the agent received; Files is how many regular files the
	// target now holds from this upload (1 without extraction).
	Bytes int64 `json:"bytes"`
	Files int   `json:"files"`
}

// Upload result states.
const (
	StorageFilesDone   = "done"
	StorageFilesFailed = "failed"
)

// StorageDownloadHead is the agent's first line on a download. On a refusal
// Code/Error are set and nothing follows.
type StorageDownloadHead struct {
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
	Dir   bool   `json:"dir"`
	// Size is the file's length; -1 for a directory (the archive's length is
	// not known in advance).
	Size int64  `json:"size"`
	Name string `json:"name"`
}
