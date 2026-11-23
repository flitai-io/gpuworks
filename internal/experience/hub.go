package experience

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/webui"
)

// hub is an offline stand-in for the HuggingFace repository API, answering
// for every model the inference catalog knows (and a typical 7B shape for
// any other id), so registering a model in the environment fills in its real
// architecture, context length and size without network access.
type hub struct {
	shapes map[string]modelShape
	assets fs.FS
}

// startHub serves the hub on a loopback port of its own.
func startHub(shapes map[string]modelShape) (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: &hub{shapes: shapes, assets: webui.FS()}, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), func() { _ = srv.Close() }, nil
}

// repoFile is one file of a simulated repository: its bytes when the hub
// serves them (config, index, README), or only a declared size (weights).
type repoFile struct {
	Path string
	Data []byte
	Size int64
}

func (h *hub) files(repo string) []repoFile {
	shape, ok := h.shapes[strings.ToLower(repo)]
	if !ok {
		shape = fallbackShape()
	}
	out := checkpoint(shape)
	if shape.Readme != "" {
		out = append(out, repoFile{Path: "README.md", Data: []byte(shape.Readme)})
	}
	return out
}

func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// Mirror the HF organization API using the avatars already packaged for
	// the console's inference catalog. Only expose files in that directory.
	if rest, ok := strings.CutPrefix(path, "/api/organizations/"); ok {
		org, ok := strings.CutSuffix(rest, "/avatar")
		if !ok || !fs.ValidPath(org) || strings.Contains(org, "/") {
			http.NotFound(w, r)
			return
		}
		file := "model-org-icons/" + strings.ToLower(org) + ".webp"
		if _, err := fs.Stat(h.assets, file); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"avatarUrl": "/" + file})
		return
	}
	if file, ok := strings.CutPrefix(path, "/model-org-icons/"); ok {
		if !fs.ValidPath(file) || strings.Contains(file, "/") || !strings.HasSuffix(file, ".webp") {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, h.assets, "model-org-icons/"+file)
		return
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/models/"); ok {
		h.modelscope(w, r, rest)
		return
	}
	// ModelScope's resolve path carries a models/ prefix; the rest is HF's.
	if rest, ok := strings.CutPrefix(path, "/models/"); ok && strings.Contains(rest, "/resolve/") {
		path = "/" + rest
	}
	// Listing: /api/models/<org>/<name>/tree/<revision>
	if rest, ok := strings.CutPrefix(path, "/api/models/"); ok {
		repo, _, found := strings.Cut(rest, "/tree/")
		if !found {
			owner, name, ok := strings.Cut(rest, "/")
			if ok && name != "" && !strings.Contains(name, "/") && owner != "" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"author": owner})
				return
			}
			http.NotFound(w, r)
			return
		}
		items := []map[string]any{}
		for _, f := range h.files(repo) {
			size := f.Size
			if f.Data != nil {
				size = int64(len(f.Data))
			}
			items = append(items, map[string]any{"type": "file", "path": f.Path, "size": size})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
		return
	}
	// File: /<org>/<name>/resolve/<revision>/<file>
	repo, rest, found := strings.Cut(strings.TrimPrefix(path, "/"), "/resolve/")
	if !found {
		http.NotFound(w, r)
		return
	}
	_, name, found := strings.Cut(rest, "/")
	if !found {
		http.NotFound(w, r)
		return
	}
	name, _ = url.PathUnescape(name)
	for _, f := range h.files(repo) {
		if f.Path == name && f.Data != nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(f.Data)
			return
		}
	}
	http.NotFound(w, r)
}

// modelscope answers ModelScope's repository API: the file listing
// (<repo>/repo/files) and one file (<repo>/repo?FilePath=).
func (h *hub) modelscope(w http.ResponseWriter, r *http.Request, rest string) {
	const revision = "master"
	if repo, ok := strings.CutSuffix(rest, "/repo/files"); ok {
		items := []map[string]any{}
		for _, f := range h.files(repo) {
			size := f.Size
			if f.Data != nil {
				size = int64(len(f.Data))
			}
			items = append(items, map[string]any{"Path": f.Path, "Name": f.Path, "Size": size, "Type": "blob", "Revision": revision, "CommittedDate": 1787357763})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": 200, "Success": true, "Data": map[string]any{
			"LatestCommitter": map[string]any{"CommittedDate": 1787357763, "ShortId": revision}, "Files": items,
		}})
		return
	}
	if repo, ok := strings.CutSuffix(rest, "/repo"); ok {
		name := r.URL.Query().Get("FilePath")
		for _, f := range h.files(repo) {
			if f.Path == name && f.Data != nil {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(f.Data)
				return
			}
		}
	}
	http.NotFound(w, r)
}
