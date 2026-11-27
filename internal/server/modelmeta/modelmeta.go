// Package modelmeta fetches a model's config.json and weight size from its
// source repo (HF/ModelScope or a plain HTTP directory index) so the estimator
// can work before the model is downloaded (deploy-form live evaluation).
// Server-side and proxy-aware.
package modelmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
	"gpuworks.ai/gpuworks/pkg/safetensors"
)

// Meta is the fetched metadata.
type Meta struct {
	// ResolvedRevision is the commit the requested ref pointed at when the
	// metadata was fetched, and is only filled in when the caller asked for no
	// particular revision. Registering it makes later downloads and node-to-node
	// syncs reproducible; an unreachable repository simply leaves it empty.
	ResolvedRevision string
	Config           *modelconfig.Config
	ConfigRaw        string
	WeightBytes      int64
	PerDtype         map[string]int64
	Arch             string
	README           string
	ContextLength    int64
	Precision        string
	OriginCreatedAt  *time.Time
	OriginUpdatedAt  *time.Time
	LogoURL          string
	// Endpoint is the address that answered, of those FetchFrom was given;
	// empty when none did, and the metadata is then empty too.
	Endpoint string
	// Answer says why config.json, the README and the weight size are all
	// unread, when the source said why; the zero value otherwise.
	Answer Answer
}

// InputModalities reports input types evidenced by config.json. A nil config
// remains unknown rather than being guessed from the model name.
func InputModalities(cfg *modelconfig.Config) []string {
	if cfg == nil {
		return nil
	}
	out := []string{"text"}
	if cfg.VisionConfig != nil {
		out = append(out, "image")
	}
	if cfg.AudioConfig != nil {
		out = append(out, "audio")
	}
	return out
}

// Fetch retrieves config.json and the weight size for a model. A non-empty
// subfolder names the one directory of the repository the model is registered
// from (a repository that holds several quantizations side by side): the
// weight size counts only the weight files under it, so a registration of one
// quantization is never sized as the whole repository.
func Fetch(ctx context.Context, source, repo, revision, subfolder, endpoint string) (*Meta, error) {
	return FetchWithClient(ctx, source, repo, revision, subfolder, endpoint, nil)
}

// FetchWithClient reads at one address - endpoint, or the source's own site
// when it is empty - through the supplied client.
func FetchWithClient(ctx context.Context, source, repo, revision, subfolder, endpoint string, base *http.Client) (*Meta, error) {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = modelrepo.BaseURL(source, "")
	}
	return FetchFrom(ctx, source, repo, revision, subfolder, []string{endpoint}, base, nil)
}

// probeTimeout bounds asking one candidate address whether it serves the
// repository, so a dead first address leaves time for the next - and does
// not hold a registration for long. The same bound as a registry probe.
const probeTimeout = 5 * time.Second

// FetchFrom reads a model's metadata from the first of candidates that
// serves the repository (planning.ServerFetchCandidates gives them, in
// order); every request of the fetch then goes to that address, through the
// supplied client. The repository record (the hubs' model API) is the
// question each candidate is asked. When none answers, the first candidate
// is still read, as the one address was before candidates: whatever it
// serves is kept. Meta.Endpoint says which address answered, if any. A
// request carries one of logins as modelrepo.AuthorizationFor allows (its
// own host, over https), as a node's downloads do.
func FetchFrom(ctx context.Context, source, repo, revision, subfolder string, candidates []string, base *http.Client, logins []modelrepo.Login) (*Meta, error) {
	subfolder, err := modelrepo.NormalizeSubfolder(subfolder)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no address to read %s %s from", source, repo)
	}
	started := time.Now()
	slog.Info("model metadata fetch started", "source", source, "repo", repo, "revision", revision, "subfolder", subfolder, "candidates", len(candidates), "first", safeMetadataURL(candidates[0]))
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	m := &Meta{}
	endpoint := candidates[0]
	// An HTTP directory is reached only through the restricted client, which
	// is what confines every metadata request to the host the directory was
	// registered with.
	client, err := sourceClient(source, endpoint, base)
	if err != nil {
		return nil, err
	}
	client = withLogins(client, logins)
	// A plain file server publishes no repository record: there is no creation
	// timestamp, no last-modified date and no owner avatar to read. That is an
	// absence, not a failure, so the fetch simply skips it rather than logging
	// a warning about an endpoint that was never going to answer.
	//
	// Of several candidates the first that serves the repository is read: its
	// record, or - for a mirror that proxies only the files - its config.json.
	// A Hugging Face-style address that shows the record of a gated
	// repository but refuses its files (a mirror without the login) is passed
	// over for one that lets them in; when every one refuses, the first that
	// answered is still read.
	var answers []Answer
	recordRead := false
	if source != modelrepo.SourceURL {
		chosen := false
		var refused string
		var refusedMeta *repositoryMeta
		for _, candidate := range candidates {
			probeCtx, cancelProbe := context.WithTimeout(ctx, probeTimeout)
			repoMeta, err := fetchRepositoryMeta(probeCtx, source, repo, candidate, client)
			if err != nil {
				slog.Warn("model metadata repository details unavailable", "source", source, "repo", repo, "endpoint", safeMetadataURL(candidate), "err", err)
				answers = append(answers, answerOf(candidate, err))
				if len(candidates) > 1 {
					_, err = getWith(probeCtx, client, modelrepo.ResolveURL(source, repo, revision, "config.json", candidate))
				}
			} else if len(candidates) > 1 && source != modelrepo.SourceMS {
				var status errHTTP
				if _, fileErr := getWith(probeCtx, client, modelrepo.ResolveURL(source, repo, revision, "config.json", candidate)); errors.As(fileErr, &status) && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
					if refused == "" {
						refused, refusedMeta = candidate, repoMeta
					}
					err = fileErr
				}
			}
			cancelProbe()
			if err != nil {
				continue
			}
			endpoint, m.Endpoint, chosen = candidate, candidate, true
			recordRead = repoMeta != nil
			m.setRepository(repoMeta)
			break
		}
		if !chosen && refused != "" {
			endpoint, m.Endpoint = refused, refused
			recordRead = true
			m.setRepository(refusedMeta)
		}
	}
	// Without a caller-chosen ref the registration would follow a moving
	// default branch. Resolve the commit it points at now so the download is
	// deterministic; a repository we cannot reach keeps the unpinned identity
	// rather than failing the registration.
	if strings.TrimSpace(revision) == "" {
		if resolved, err := modelrepo.ResolveRevision(ctx, source, repo, revision, endpoint, nil, modelrepo.RequestOptions{Client: client}); err == nil {
			m.ResolvedRevision = resolved
			slog.Info("model metadata resolved default revision", "source", source, "repo", repo, "revision", resolved)
		} else {
			slog.Warn("model metadata default revision unresolved", "source", source, "repo", repo, "err", err)
		}
	}

	// config.json
	cfgURL := modelrepo.ResolveURL(source, repo, revision, "config.json", endpoint)
	if body, err := getWith(ctx, client, cfgURL); err == nil {
		m.ConfigRaw = string(body)
		if cfg, perr := modelconfig.Parse(strings.NewReader(string(body))); perr == nil {
			m.Config = cfg
			m.Arch = archOf(cfg)
			m.ContextLength = ContextLength(cfg, m.ConfigRaw)
			m.Precision = Precision(cfg, m.ConfigRaw)
		} else {
			slog.Warn("model metadata config parse failed", "url", safeMetadataURL(cfgURL), "bytes", len(body), "err", perr)
		}
	} else {
		slog.Warn("model metadata config unavailable", "url", safeMetadataURL(cfgURL), "err", err)
	}
	readmeURL := modelrepo.ResolveURL(source, repo, revision, "README.md", endpoint)
	if body, err := getWith(ctx, client, readmeURL); err == nil {
		m.README = string(body)
		if len(m.README) > 512*1024 {
			m.README = m.README[:512*1024]
		}
	} else {
		slog.Warn("model metadata README unavailable", "url", safeMetadataURL(readmeURL), "err", err)
	}

	// weight size: prefer the fast path (index.json metadata.total_size). The
	// repository root's index sizes the whole repository, so a subfolder
	// consults only an index of its own.
	idxURL := modelrepo.ResolveURL(source, repo, revision, subfolderPath(subfolder, "model.safetensors.index.json"), endpoint)
	if body, err := getWith(ctx, client, idxURL); err == nil {
		if info, perr := safetensors.ParseIndexJSON(strings.NewReader(string(body))); perr == nil && info.TotalBytes > 0 {
			m.WeightBytes = info.TotalBytes
			slog.Info("model metadata weight index parsed", "url", safeMetadataURL(idxURL), "bytes", len(body), "weight_bytes", m.WeightBytes)
		} else if perr != nil {
			slog.Warn("model metadata weight index parse failed", "url", safeMetadataURL(idxURL), "bytes", len(body), "err", perr)
		} else {
			slog.Warn("model metadata weight index had no total", "url", safeMetadataURL(idxURL), "bytes", len(body))
		}
	} else {
		slog.Info("model metadata weight index unavailable; falling back to repository listing", "url", safeMetadataURL(idxURL), "err", err)
	}
	var listingErr error
	if m.WeightBytes == 0 {
		// Fallback: sum the .safetensors file sizes from the repo listing.
		if files, err := modelrepo.ListFiles(ctx, source, repo, revision, endpoint, nil, modelrepo.RequestOptions{Client: client}); err == nil {
			scoped := modelrepo.FilesUnderSubfolder(files, subfolder)
			if len(scoped) == 0 {
				slog.Warn("model metadata weight listing has no files under the subfolder", "source", source, "repo", repo, "subfolder", subfolder, "files", len(files))
			}
			selected := selectWeightFiles(scoped)
			var total int64
			for _, file := range selected {
				total += file.Size
			}
			m.WeightBytes = exactListingWeightBytes(ctx, client, source, revision, endpoint, repo, selected, total)
			slog.Info("model metadata weight listing parsed", "source", source, "repo", repo, "subfolder", subfolder, "files", len(files), "weight_files", len(selected), "weight_bytes", m.WeightBytes)
		} else {
			slog.Warn("model metadata weight listing failed", "source", source, "repo", repo, "err", err)
			listingErr = err
		}
	}
	switch read := m.ConfigRaw != "" || m.README != "" || m.WeightBytes > 0; {
	case read && m.Endpoint == "":
		m.Endpoint = endpoint
	case read:
	case recordRead:
		// The site showed the repository: a 404 for its files is about the
		// revision, and a refusal is about the files.
		switch answer := answerOf(endpoint, listingErr); answer.Outcome {
		case OutcomeRepoNotFound:
			answer.Outcome = OutcomeRevisionNotFound
			m.Answer = answer
		case OutcomeDenied:
			m.Answer = answer
		}
	default:
		m.Answer = unanswered(answers)
	}
	slog.Info("model metadata fetch finished", "source", source, "repo", repo, "revision", revision, "endpoint", safeMetadataURL(m.Endpoint), "weight_bytes", m.WeightBytes, "config", m.Config != nil, "outcome", string(m.Answer.Outcome), "duration", time.Since(started).String())
	return m, nil
}

// subfolderPath is name inside the subfolder, or name itself without one.
func subfolderPath(subfolder, name string) string {
	if subfolder == "" {
		return name
	}
	return subfolder + "/" + name
}

// weightListingTotal sums the weight files a listing contributes to the model.
func weightListingTotal(files []modelrepo.File) (int64, int) {
	selected := selectWeightFiles(files)
	var total int64
	for _, f := range selected {
		total += f.Size
	}
	return total, len(selected)
}

// selectWeightFiles avoids counting two alternative serialization layouts as
// one model. Mistral repositories commonly publish consolidated.safetensors
// next to an HF-style sharded set; the runtime selects one layout, so the
// planning estimate must do the same. Prefer the consolidated layout when it
// is present, otherwise take the ordinary sharded files. A repository with
// neither is weighed by its GGUF files, so a GGUF-only model is not counted as
// weightless.
func selectWeightFiles(files []modelrepo.File) []modelrepo.File {
	hasConsolidated := false
	for _, f := range files {
		base := strings.ToLower(filepath.Base(f.Path))
		if strings.HasPrefix(base, "consolidated") && (strings.HasSuffix(base, ".safetensors") || strings.HasSuffix(base, ".bin")) {
			hasConsolidated = true
			break
		}
	}
	selected := make([]modelrepo.File, 0, len(files))
	for _, f := range files {
		base := strings.ToLower(filepath.Base(f.Path))
		if !(strings.HasSuffix(base, ".safetensors") || strings.HasSuffix(base, ".bin")) {
			continue
		}
		if hasConsolidated && !strings.HasPrefix(base, "consolidated") {
			continue
		}
		selected = append(selected, f)
	}
	if len(selected) == 0 {
		for _, f := range files {
			if strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
				selected = append(selected, f)
			}
		}
	}
	return selected
}

// ContextLength returns the smallest positive context limit advertised by the
// model config. Different model families use different keys, and honoring the
// minimum avoids over-promising a deploy form's max_model_len.
func ContextLength(cfg *modelconfig.Config, raw string) int64 {
	values := make([]int64, 0, 6)
	if cfg != nil && cfg.MaxPositionEmbeddings > 0 {
		values = append(values, int64(cfg.MaxPositionEmbeddings))
	}
	var object map[string]any
	if json.Unmarshal([]byte(raw), &object) == nil {
		collectContextValues(object, &values)
	}
	if len(values) == 0 {
		return 0
	}
	minimum := values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
	}
	return minimum
}

func collectContextValues(object map[string]any, values *[]int64) {
	for _, key := range []string{"max_position_embeddings", "max_sequence_length", "model_max_length", "seq_length", "n_positions", "context_length"} {
		if raw, ok := object[key]; ok {
			switch number := raw.(type) {
			case float64:
				if number > 0 {
					*values = append(*values, int64(number))
				}
			case json.Number:
				if parsed, err := number.Int64(); err == nil && parsed > 0 {
					*values = append(*values, parsed)
				}
			}
		}
	}
	for _, key := range []string{"text_config", "thinker_config"} {
		if nested, ok := object[key].(map[string]any); ok {
			collectContextValues(nested, values)
		}
	}
}

// Precision combines torch_dtype with common quantization_config metadata.
// The raw config is retained so unknown quantization fields remain available
// without widening the shared normalized modelconfig struct.
func Precision(cfg *modelconfig.Config, raw string) string {
	dtype := ""
	if cfg != nil {
		dtype = strings.TrimSpace(cfg.TorchDtype)
	}
	method, bits := "", 0
	var object map[string]any
	if json.Unmarshal([]byte(raw), &object) == nil {
		method, bits = quantization(object)
	}
	quant := method
	if bits > 0 {
		if quant != "" {
			quant += " "
		}
		quant += fmt.Sprintf("%dbit", bits)
	}
	if dtype != "" && quant != "" {
		return dtype + " / " + quant
	}
	if dtype != "" {
		return dtype
	}
	return quant
}

func quantization(object map[string]any) (string, int) {
	for _, key := range []string{"quantization_config", "text_config"} {
		if nested, ok := object[key].(map[string]any); ok {
			method := ""
			if value, ok := nested["quant_method"].(string); ok {
				method = strings.TrimSpace(value)
			}
			if method == "" {
				if value, ok := nested["quantization_method"].(string); ok {
					method = strings.TrimSpace(value)
				}
			}
			bits := 0
			if value, ok := nested["bits"].(float64); ok {
				bits = int(value)
			}
			if method != "" || bits > 0 {
				return method, bits
			}
		}
	}
	return "", 0
}

func archOf(cfg *modelconfig.Config) string {
	if cfg == nil {
		return ""
	}
	if len(cfg.Architectures) > 0 {
		return cfg.Architectures[0]
	}
	return cfg.ModelType
}

func getWith(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	started := time.Now()
	logURL := safeMetadataURL(url)
	slog.Info("model metadata HTTP request started", "url", logURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.Warn("model metadata HTTP request rejected", "url", logURL, "err", err)
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("model metadata HTTP request failed", "url", logURL, "duration", time.Since(started).String(), "err", err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("model metadata HTTP response unexpected", "url", logURL, "status", resp.StatusCode, "duration", time.Since(started).String())
		return nil, errHTTP(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		slog.Warn("model metadata HTTP response read failed", "url", logURL, "status", resp.StatusCode, "duration", time.Since(started).String(), "bytes", len(body), "err", err)
		return nil, err
	}
	slog.Info("model metadata HTTP request finished", "url", logURL, "status", resp.StatusCode, "bytes", len(body), "duration", time.Since(started).String())
	return body, nil
}

func safeMetadataURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// setRepository keeps what the repository record says, when there is one.
func (m *Meta) setRepository(repoMeta *repositoryMeta) {
	if repoMeta == nil {
		return
	}
	m.OriginCreatedAt = repoMeta.CreatedAt
	m.OriginUpdatedAt = repoMeta.UpdatedAt
	m.LogoURL = repoMeta.LogoURL
}

type errHTTP int

func (e errHTTP) Error() string { return "http " + itoa(int(e)) }

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// withLogins is client with logins attached to its requests.
func withLogins(client *http.Client, logins []modelrepo.Login) *http.Client {
	if len(logins) == 0 {
		return client
	}
	out := &http.Client{}
	if client != nil {
		*out = *client
	}
	next := out.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	out.Transport = loginTransport{next: next, logins: logins}
	return out
}

// loginTransport adds a login's Authorization header to a request it applies
// to. A redirect the client follows to another host is not given one: the
// client forwards the original header only to the same domain and its
// subdomains, and the logins match exact hosts.
type loginTransport struct {
	next   http.RoundTripper
	logins []modelrepo.Login
}

func (t loginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == "" {
		if header := modelrepo.AuthorizationFor(req.URL.String(), t.logins); header != "" {
			req = req.Clone(req.Context())
			req.Header.Set("Authorization", header)
		}
	}
	return t.next.RoundTrip(req)
}
