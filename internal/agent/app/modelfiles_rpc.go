package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

type registerPathParams struct {
	Path          string `json:"path"`
	ParseMetadata bool   `json:"inspect_metadata,omitempty"`
	StorageID     uint   `json:"storage_id,omitempty"`
}

type registerPathResult struct {
	Path      string                `json:"path"`
	SizeBytes int64                 `json:"size_bytes"`
	Metadata  *modelmetadata.Report `json:"metadata,omitempty"`
	// Revision is the commit a huggingface download left in the directory's
	// metadata, when all its files share one (modelscan.HuggingFaceCommit).
	Revision string `json:"revision,omitempty"`
}

func (n *NodeRuntime) rpcRegisterPath(ctx context.Context, params json.RawMessage) (any, error) {
	var p registerPathParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	p.Path = filepath.Clean(p.Path)
	if p.Path == "." || p.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	var result registerPathResult
	var root string
	if p.StorageID != 0 {
		var err error
		root, _, err = n.readyMount(p.StorageID)
		if err != nil {
			return nil, err
		}
	}
	readModelDirectory := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var report modelmetadata.Report
		var err error
		// Registration adopts a user-supplied directory rather than a verified
		// download manifest. Recognize this exact directory before walking its
		// bytes, even when the server already has metadata for the model.
		if root != "" {
			report, err = modelscan.ParseMetadataWithin(ctx, root, p.Path)
		} else {
			report, err = modelscan.ParseMetadata(ctx, p.Path)
		}
		if err != nil {
			return err
		}
		if err := report.ValidateSingleEntry(); err != nil {
			return err
		}
		if p.ParseMetadata {
			result.Metadata = &report
		}
		info, err := os.Stat(p.Path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("path is not a directory: %s", p.Path)
		}
		size, err := validateModelDir(p.Path)
		if err != nil {
			return err
		}
		result.Path, result.SizeBytes = p.Path, size
		result.Revision = modelscan.HuggingFaceCommit(p.Path)
		return ctx.Err()
	}
	var err error
	if p.StorageID != 0 {
		err = n.storageDo(p.StorageID, "parse_metadata", 50*time.Second+n.storageTimeouts().scanGrace, readModelDirectory)
	} else {
		err = readModelDirectory()
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// validateModelDir is the finished-model-directory check; modelscan owns it so
// every node-local path (download, copy, peer sync, registration) judges a
// directory the same way.
func validateModelDir(dir string) (int64, error) { return modelscan.ValidateModelDir(dir) }

// rpcFSList lists a directory for the file browser / model registration RPC.
type fsListParams struct {
	Path string `json:"path"`
}

type fsEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

func (n *NodeRuntime) rpcFSList(params json.RawMessage) (any, error) {
	var p fsListParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		p.Path = "/"
	}
	entries, err := os.ReadDir(p.Path)
	if err != nil {
		return nil, err
	}
	out := make([]fsEntry, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		out = append(out, fsEntry{Name: e.Name(), IsDir: e.IsDir(), Size: size})
	}
	return map[string]any{"path": p.Path, "entries": out}, nil
}

type fsReadFileParams struct {
	Path     string `json:"path"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
	// MissingOK asks for an absent file to be answered as {"missing": true}
	// rather than as an error, for a caller to whom the file is optional: an
	// RPC error cannot tell "not there" from "could not look".
	MissingOK bool `json:"missing_ok,omitempty"`
}

// rpcFSStat answers existence and file type for one path. It is deliberately
// not "stat anything the server likes for its own sake": the caller uses it to
// check a candidate file it already knows the name of, before either adopting
// the path or transferring its bytes.
func (n *NodeRuntime) rpcFSStat(params json.RawMessage) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	p.Path = filepath.Clean(strings.TrimSpace(p.Path))
	if p.Path == "." || p.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	info, err := os.Stat(p.Path)
	if errors.Is(err, fs.ErrNotExist) {
		// Same sentence os.Stat writes, plus the code the server reads.
		return nil, &proto.RPCError{Code: proto.ErrorPathNotFound, Message: err.Error(), Params: map[string]any{"path": p.Path}}
	}
	if err != nil {
		return nil, err
	}
	return proto.FSStatResult{Path: p.Path, SizeBytes: info.Size(), Regular: info.Mode().IsRegular(), Dir: info.IsDir()}, nil
}

func (n *NodeRuntime) rpcModelEvict(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
		return nil, fmt.Errorf("model eviction requires path")
	}
	target := filepath.Clean(n.resolveModelPath(p.Path))
	inside := false
	for _, root := range n.storageRoots() {
		if pathWithin(root, target) && target != root {
			inside = true
			break
		}
	}
	if !inside {
		return nil, fmt.Errorf("model eviction path is outside models root")
	}
	// A task parked for this directory's storage would download the model
	// again the day the storage recovers.
	n.dropWaitingUnder(target)
	if err := n.removeModelPath(ctx, target); err != nil {
		return nil, err
	}
	return map[string]any{"path": target, "removed": true}, nil
}

// removeModelPath deletes a model directory once nothing is downloading into
// it. Downloads whose target overlaps the directory are canceled and waited
// for first; the removal itself runs under dlMu, so no download for the path
// can start between the last one ending and the directory going away. A
// download that does not stop before ctx ends leaves the directory untouched
// and fails the eviction rather than deleting files a writer still holds.
func (n *NodeRuntime) removeModelPath(ctx context.Context, target string) error {
	return n.removeModelPathWith(ctx, target, nil, func() error { return os.RemoveAll(target) })
}

// beforeStop runs under dlMu before touching writers. A completed eviction can
// therefore acknowledge a retry without canceling a newer download at its path.
func (n *NodeRuntime) removeModelPathWith(ctx context.Context, target string, beforeStop func() (bool, error), remove func() error) error {
	for {
		n.dlMu.Lock()
		if beforeStop != nil {
			if done, err := beforeStop(); done || err != nil {
				n.dlMu.Unlock()
				return err
			}
		}
		var stopping []*modelDownload
		var keys []string
		for key, active := range n.downloading {
			if pathWithin(target, active.target) || pathWithin(active.target, target) {
				active.cancel()
				stopping = append(stopping, active)
				keys = append(keys, key)
			}
		}
		if len(stopping) == 0 {
			err := remove()
			n.dlMu.Unlock()
			return err
		}
		n.dlMu.Unlock()
		for i, active := range stopping {
			select {
			case <-active.done:
			case <-ctx.Done():
				return fmt.Errorf("model download into %s did not stop: %w", target, ctx.Err())
			}
			// The canceled download reports nothing more, but a progress
			// sample it queued earlier would still be flushed and overwrite
			// whatever the server records for this model next.
			n.progress.discard(modelFileProgressKey(keys[i]))
		}
	}
}

// pathWithin reports whether path is dir or lies below it. Both must be clean.
func pathWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rpcFSReadFile is intentionally bounded: the server only needs config.json
// and a README preview, never an arbitrary large file or a model weight.
func (n *NodeRuntime) rpcFSReadFile(params json.RawMessage) (any, error) {
	var p fsReadFileParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	p.Path = filepath.Clean(strings.TrimSpace(p.Path))
	if p.Path == "." || p.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if p.MaxBytes <= 0 || p.MaxBytes > 512*1024 {
		p.MaxBytes = 512 * 1024
	}
	info, err := os.Stat(p.Path)
	if err != nil {
		if p.MissingOK && errors.Is(err, fs.ErrNotExist) {
			return map[string]any{"path": p.Path, "missing": true}, nil
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file: %s", p.Path)
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, p.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	truncated := int64(len(b)) > p.MaxBytes
	if truncated {
		b = b[:p.MaxBytes]
	}
	return map[string]any{"path": p.Path, "content": string(b), "size_bytes": info.Size(), "truncated": truncated}, nil
}
