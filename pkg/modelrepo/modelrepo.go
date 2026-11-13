// Package modelrepo lists and resolves model files from HuggingFace and
// ModelScope repositories and from plain HTTP directory indexes. It is shared
// by the server metadata fetcher and the agent downloader; it contains no
// agent or server-specific behavior.
package modelrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// NormalizeSubfolder validates one repository-relative download prefix. The
// prefix remains part of every local path; callers must never use it as an
// unchecked filesystem path or reinterpret it as the model root.
func NormalizeSubfolder(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.Contains(value, "\\") {
		return "", fmt.Errorf("model repository subfolder %q must use forward slashes", value)
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || path.IsAbs(value) || clean != value || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe model repository subfolder %q", value)
	}
	return clean, nil
}

// FilesUnderSubfolder keeps the files of one repository subtree, with their
// repository-relative paths in forward-slash form. An empty subfolder is the whole
// repository. The subfolder is matched as a directory prefix, so "IQ2_XS" never
// selects "IQ2_XS_extra/...". When no directory has that name, a file at exactly
// that path is the selection, so one adapter file can be taken from a repository
// that keeps many at its root; a directory wins over a file of the same name.
// The caller validates the subfolder with NormalizeSubfolder first.
func FilesUnderSubfolder(files []File, subfolder string) []File {
	if subfolder == "" {
		return files
	}
	prefix := subfolder + "/"
	out := make([]File, 0, len(files))
	var exact []File
	for _, f := range files {
		// Some hubs list paths with backslashes; the repository's own form is
		// forward slashes, which is how a kept file is returned.
		f.Path = strings.ReplaceAll(f.Path, "\\", "/")
		if strings.HasPrefix(f.Path, prefix) && f.Path != prefix {
			out = append(out, f)
		} else if f.Path == subfolder {
			exact = append(exact, f)
		}
	}
	if len(out) == 0 {
		return exact
	}
	return out
}

// Sources.
const (
	SourceHF = "huggingface"
	SourceMS = "modelscope"
	// SourceURL is a plain HTTP directory index: an ordinary file server with
	// no repository API, no revisions and no resolve endpoint. The directory
	// URL itself is the endpoint, and the repository id is identity only.
	SourceURL = "url"
)

// File is one file in a repo.
type File struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Revision string `json:"revision,omitempty"` // Pinned repository commit, when provided.
}

// RequestOptions controls the HTTP request used to list repository files.
// Client and Rewrite are intentionally injectable so an agent can apply the
// node's proxy and URL acceleration settings to metadata requests as well as
// file downloads. A nil client uses http.DefaultClient and a nil rewrite keeps
// the URL unchanged.
type RequestOptions struct {
	Client  *http.Client
	Rewrite func(string) string
}

// The hubs' own addresses. BaseURL is the only reader; everything that needs
// where a source lives asks it.
const (
	huggingFaceBase = "https://huggingface.co"
	modelScopeBase  = "https://modelscope.cn"
)

// BaseURL is the address a model's bytes live at when nothing stands in
// front of it: the hub's own site, or for a plain HTTP directory the
// directory itself (sourceURL). Pull source rules substitute other
// addresses for it; a caller that reaches a hub some other way passes that
// address as the endpoint of the calls below. Empty for an unknown source.
func BaseURL(source, sourceURL string) string {
	switch source {
	case SourceHF:
		return huggingFaceBase
	case SourceMS:
		return modelScopeBase
	case SourceURL:
		return strings.TrimRight(strings.TrimSpace(sourceURL), "/")
	}
	return ""
}

// StatusError is a hub or file server answering with an error status: an
// HTTP status other than 200, or ModelScope's own Code in a 200 response.
// Callers that walk several addresses tell "this address cannot serve it"
// from other failures by it, never by the message text.
type StatusError struct {
	URL  string
	Code int
	// Detail is the whole message when the hub explained itself.
	Detail string
}

func (e *StatusError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return fmt.Sprintf("%s: http %d", e.URL, e.Code)
}

// hfBase and msBase are the address a call is made against: the caller's
// endpoint, or the hub's own site.
func hfBase(endpoint string) string {
	if endpoint != "" {
		return strings.TrimRight(endpoint, "/")
	}
	return BaseURL(SourceHF, "")
}

func msBase(endpoint string) string {
	if endpoint != "" {
		return strings.TrimRight(endpoint, "/")
	}
	return BaseURL(SourceMS, "")
}

// Origin is the host a download from source is made from: the endpoint (for
// a plain HTTP directory, the directory itself) or the source's own site,
// after the caller's rewrite, which is how a mirror takes over. Empty for an
// unknown source or an address that does not parse.
func Origin(source, endpoint string, options ...RequestOptions) string {
	var base string
	switch source {
	case SourceMS:
		base = msBase(endpoint)
	case SourceHF:
		base = hfBase(endpoint)
	case SourceURL:
		base = strings.TrimSpace(endpoint)
	default:
		return ""
	}
	if len(options) > 0 && options[0].Rewrite != nil {
		base = options[0].Rewrite(base)
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// ListFiles enumerates the files in a repo at a revision.
func ListFiles(ctx context.Context, source, repo, revision, endpoint string, headers map[string]string, options ...RequestOptions) ([]File, error) {
	switch source {
	case SourceMS:
		return listModelScope(ctx, repo, revision, endpoint, headers, options...)
	case SourceURL:
		return listHTTPDir(ctx, endpoint, headers, options...)
	case SourceHF:
		return listHF(ctx, repo, revision, endpoint, headers, options...)
	default:
		// An unknown source used to be listed as if it were Hugging Face,
		// which turned a source an old binary does not understand into a
		// confusing 404 against the wrong host instead of a clear refusal.
		return nil, fmt.Errorf("unsupported model source %q: url=%s repo=%s", source, endpoint, repo)
	}
}

// ResolveURL builds the download URL for one file.
func ResolveURL(source, repo, revision, path, endpoint string) string {
	switch source {
	case SourceURL:
		// The endpoint is the directory itself, so a file's URL is just the
		// directory plus its listed path.
		return resolveHTTPDirURL(endpoint, path)
	case SourceMS:
		if strings.TrimSpace(revision) == "" {
			// ModelScope's API download endpoint follows the repository's default
			// branch when Revision is omitted. The resolve endpoint would silently
			// turn an omitted revision into the literal "main", which is not the
			// platform-default semantics for every repository.
			return fmt.Sprintf("%s/api/v1/models/%s/repo?FilePath=%s", msBase(endpoint), repo, url.QueryEscape(path))
		}
		return fmt.Sprintf("%s/models/%s/resolve/%s/%s", msBase(endpoint), repo, revision, path)
	default:
		if strings.TrimSpace(revision) == "" {
			revision = "main"
		}
		return fmt.Sprintf("%s/%s/resolve/%s/%s", hfBase(endpoint), repo, revision, path)
	}
}

// ProbeFileURL is the file a probe of a Hugging Face-style address asks for
// after the repository answered: a gated repository shows its record to
// anyone and refuses only its files, so only a file tells whether a download
// would be let in. "" for ModelScope, whose record already refuses a private
// repository.
func ProbeFileURL(source, repo, base string) string {
	if source == SourceMS {
		return ""
	}
	return fmt.Sprintf("%s/%s/resolve/main/config.json", hfBase(base), repo)
}

// ProbeURL is the one request that tells whether base serves repo: the
// hub's own answer about the repository (Hugging Face's revision endpoint,
// ModelScope's model record). A bare HEAD of the site would not do - a
// captive portal answers it too.
func ProbeURL(source, repo, base string) string {
	if source == SourceMS {
		return fmt.Sprintf("%s/api/v1/models/%s", msBase(base), repo)
	}
	return fmt.Sprintf("%s/api/models/%s/revision/main", hfBase(base), repo)
}

// ResolveRevision resolves which commit the repository's default branch (or
// the given branch/ref) currently points at. Callers use it to pin a
// registration to an immutable commit so later downloads and node-to-node
// syncs describe the same bytes; an unresolvable repository is not an error
// the caller has to fail on, it simply keeps the unpinned identity.
func ResolveRevision(ctx context.Context, source, repo, revision, endpoint string, headers map[string]string, options ...RequestOptions) (string, error) {
	switch source {
	case SourceURL:
		// A directory index has no commit history to pin to. That is not a
		// failure the caller has to handle: the model simply keeps the
		// unpinned identity, exactly as an unresolvable repository does.
		return "", nil
	case SourceMS:
		return resolveModelScopeRevision(ctx, repo, revision, endpoint, headers, options...)
	default:
		return resolveHFRevision(ctx, repo, revision, endpoint, headers, options...)
	}
}

type hfRevisionResp struct {
	SHA string `json:"sha"`
}

func resolveHFRevision(ctx context.Context, repo, revision, endpoint string, headers map[string]string, options ...RequestOptions) (string, error) {
	if strings.TrimSpace(revision) == "" {
		revision = "main"
	}
	requestURL := fmt.Sprintf("%s/api/models/%s/revision/%s", hfBase(endpoint), repo, revision)
	body, err := getJSON(ctx, requestURL, headers, options...)
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return "", fmt.Errorf("huggingface resolve revision returned an empty response: url=%s source=%s revision=%q", effectiveURL(requestURL, options...), SourceHF, revision)
	}
	var resp hfRevisionResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse hf revision: %w", err)
	}
	sha := strings.TrimSpace(resp.SHA)
	if sha == "" {
		return "", fmt.Errorf("huggingface resolve revision returned no commit: url=%s source=%s revision=%q", effectiveURL(requestURL, options...), SourceHF, revision)
	}
	return sha, nil
}

func resolveModelScopeRevision(ctx context.Context, repo, revision, endpoint string, headers map[string]string, options ...RequestOptions) (string, error) {
	resp, requestURL, err := fetchModelScopeFiles(ctx, repo, revision, endpoint, headers, options...)
	if err != nil {
		return "", err
	}
	pinned := strings.TrimSpace(modelScopePinnedRevision(resp))
	if pinned == "" {
		return "", fmt.Errorf("modelscope resolve revision returned no commit: url=%s source=%s revision=%q", effectiveURL(requestURL, options...), SourceMS, revision)
	}
	return pinned, nil
}

type hfTreeItem struct {
	Type string `json:"type"` // file | directory
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func listHF(ctx context.Context, repo, revision, endpoint string, headers map[string]string, options ...RequestOptions) ([]File, error) {
	if strings.TrimSpace(revision) == "" {
		revision = "main"
	}
	url := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true", hfBase(endpoint), repo, revision)
	body, err := getJSON(ctx, url, headers, options...)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("huggingface list files returned an empty response: url=%s source=%s revision=%q", effectiveURL(url, options...), SourceHF, revision)
	}
	var items []hfTreeItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("parse hf tree: %w", err)
	}
	files := make([]File, 0, len(items))
	for _, item := range items {
		if item.Type == "file" {
			files = append(files, File{Path: item.Path, Size: item.Size})
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("huggingface list files returned no files: url=%s source=%s revision=%q", effectiveURL(url, options...), SourceHF, revision)
	}
	return files, nil
}

type msFilesResp struct {
	Code    int    `json:"Code"`
	Message string `json:"Message"`
	Data    struct {
		LatestCommitter struct {
			CommittedDate int64  `json:"CommittedDate"`
			ShortID       string `json:"ShortId"`
		} `json:"LatestCommitter"`
		Files []struct {
			Path          string `json:"Path"`
			Name          string `json:"Name"`
			Size          int64  `json:"Size"`
			Type          string `json:"Type"`     // tree | blob
			Revision      string `json:"Revision"` // commit that last changed this file
			CommittedDate int64  `json:"CommittedDate"`
		} `json:"Files"`
	} `json:"Data"`
}

// fetchModelScopeFiles performs the one repository listing request that both
// the file listing and the revision resolution need, so the two can never
// disagree about the request shape or the platform's error envelope.
func fetchModelScopeFiles(ctx context.Context, repo, revision, endpoint string, headers map[string]string, options ...RequestOptions) (msFilesResp, string, error) {
	requestURL := fmt.Sprintf("%s/api/v1/models/%s/repo/files?Recursive=true", msBase(endpoint), repo)
	if strings.TrimSpace(revision) != "" {
		requestURL += "&Revision=" + url.QueryEscape(strings.TrimSpace(revision))
	}
	var resp msFilesResp
	body, err := getJSON(ctx, requestURL, headers, options...)
	if err != nil {
		return resp, requestURL, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return resp, requestURL, fmt.Errorf("modelscope list files returned an empty response: url=%s source=%s revision=%q", effectiveURL(requestURL, options...), SourceMS, revision)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return resp, requestURL, fmt.Errorf("parse ms files: %w", err)
	}
	if resp.Code != http.StatusOK {
		message := strings.TrimSpace(resp.Message)
		if message == "" {
			message = "unknown ModelScope API error"
		}
		shown := effectiveURL(requestURL, options...)
		return resp, requestURL, &StatusError{URL: shown, Code: resp.Code, Detail: fmt.Sprintf("modelscope list files: code=%d message=%s url=%s source=%s revision=%q", resp.Code, message, shown, SourceMS, revision)}
	}
	return resp, requestURL, nil
}

func listModelScope(ctx context.Context, repo, revision, endpoint string, headers map[string]string, options ...RequestOptions) ([]File, error) {
	resp, requestURL, err := fetchModelScopeFiles(ctx, repo, revision, endpoint, headers, options...)
	if err != nil {
		return nil, err
	}
	if len(resp.Data.Files) == 0 {
		return nil, fmt.Errorf("modelscope list files returned no files: url=%s source=%s revision=%q", effectiveURL(requestURL, options...), SourceMS, revision)
	}
	pinnedRevision := modelScopePinnedRevision(resp)
	files := make([]File, 0, len(resp.Data.Files))
	for _, file := range resp.Data.Files {
		if file.Type != "blob" {
			continue
		}
		path := file.Path
		if path == "" {
			path = file.Name
		}
		files = append(files, File{Path: path, Size: file.Size, Revision: pinnedRevision})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("modelscope list files returned no files: url=%s source=%s revision=%q", effectiveURL(requestURL, options...), SourceMS, revision)
	}
	return files, nil
}

func modelScopePinnedRevision(resp msFilesResp) string {
	shortID := strings.TrimSpace(resp.Data.LatestCommitter.ShortID)
	latestDate := resp.Data.LatestCommitter.CommittedDate
	var newestRevision string
	var newestDate int64
	for _, file := range resp.Data.Files {
		revision := strings.TrimSpace(file.Revision)
		if revision == "" {
			continue
		}
		if latestDate > 0 && file.CommittedDate == latestDate && (shortID == "" || strings.HasPrefix(revision, shortID)) {
			return revision
		}
		if newestRevision == "" || file.CommittedDate > newestDate {
			newestRevision, newestDate = revision, file.CommittedDate
		}
	}
	// A deletion-only latest commit may not be attached to any remaining file.
	// ModelScope accepts short commit IDs in resolve URLs, so retaining the
	// LatestCommitter short ID is still safer than falling back to a branch name.
	if shortID != "" && latestDate > newestDate {
		return shortID
	}
	if newestRevision != "" {
		return newestRevision
	}
	return shortID
}

func getJSON(ctx context.Context, rawURL string, headers map[string]string, options ...RequestOptions) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rawURL = effectiveURL(rawURL, options...)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := http.DefaultClient
	if len(options) > 0 && options[0].Client != nil {
		client = options[0].Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{URL: rawURL, Code: resp.StatusCode}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func effectiveURL(rawURL string, options ...RequestOptions) string {
	if len(options) > 0 && options[0].Rewrite != nil {
		return options[0].Rewrite(rawURL)
	}
	return rawURL
}
