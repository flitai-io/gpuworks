package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/benchrun"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Tokenizer sources. A run stores only the reference a source resolves to, so
// the source itself is an input to creation rather than persisted state: the
// reference is self-describing (hf://, ms://, or a path) and is what the detail
// page and the report already display.
const (
	benchmarkTokenizerSourceAuto        = "auto"
	benchmarkTokenizerSourceEstimate    = "estimate"
	benchmarkTokenizerSourceHuggingFace = "huggingface"
	benchmarkTokenizerSourceModelScope  = "modelscope"
	benchmarkTokenizerSourceUpload      = "upload"
	// benchmarkTokenizerSourceEngine is the target's own tokenizer, reached
	// through its /v1/tokenize and /v1/detokenize (benchrun.EngineTokenizerRef).
	benchmarkTokenizerSourceEngine = "engine"
)

const (
	// benchmarkTokenizerStatTimeout bounds the existence check of a candidate
	// node-local path. It is a stat on a connected agent, not a transfer.
	benchmarkTokenizerStatTimeout = 10 * time.Second
	// benchmarkTokenizerFetchIdle is how long a fetch may stall between two
	// reads before it is abandoned. It is an idle bound rather than a deadline
	// for the whole transfer, so a large tokenizer on a slow link still
	// completes while a wedged node does not hold the request open.
	benchmarkTokenizerFetchIdle = 30 * time.Second
	// benchmarkTokenizerFetchedPrefix names the content-addressed copies pulled
	// back from a node. Same content, same name: a rerun of one model reuses the
	// copy instead of crossing the network again.
	benchmarkTokenizerFetchedPrefix = "tokenizer-fetched-"
)

// benchmarkTokenizerRequest is what the tokenizer decision needs: the source
// the client named, and what target resolution already learned about the target
// (nil deployment and node mean an external endpoint, which has neither).
type benchmarkTokenizerRequest struct {
	Source     string
	Reference  string
	TargetKind string
	LoadOrigin string
	Deployment *store.Deployment
	Node       *store.Node
}

// resolveBenchmarkTokenizer turns a requested source into the one reference the
// run stores. The two repository sources and upload carry their reference from
// the client; estimate declares that no tokenizer is used; auto resolves one.
//
// An invalid source is refused instead of ignored: guessing what a client meant
// is how a run ends up measuring with a tokenizer nobody chose.
func (a *apiHandlers) resolveBenchmarkTokenizer(ctx context.Context, req benchmarkTokenizerRequest) (string, error) {
	switch req.Source {
	case benchmarkTokenizerSourceEstimate:
		// An explicit estimate is a decision, not a gap to fill. Filling it in
		// would put the run in exact mode while the user believes it is
		// estimating — and would send the node to the network for it.
		return "", nil
	case benchmarkTokenizerSourceHuggingFace, benchmarkTokenizerSourceModelScope:
		return repositoryTokenizerReference(req.Source, req.Reference)
	case benchmarkTokenizerSourceUpload:
		reference := strings.TrimSpace(req.Reference)
		if reference == "" {
			return "", benchmarkParameterError("tokenizer_ref", "the selected tokenizer source requires a repository or uploaded tokenizer reference")
		}
		return reference, nil
	case benchmarkTokenizerSourceEngine:
		// Named on its own, the engine is the only source: a target that does
		// not answer stops the run before load rather than switching to a
		// tokenizer nobody chose.
		if req.TargetKind == "instance" && req.Deployment != nil && req.Deployment.TokenizeDisabled {
			return "", productError(ErrorBenchmarkTokenizerSourceInvalid,
				"tokenize is turned off for this deployment; turn it on or choose another tokenizer source", map[string]any{"source": req.Source})
		}
		return benchrun.EngineTokenizerRef, nil
	case benchmarkTokenizerSourceAuto:
		if req.TargetKind != "instance" {
			// An external endpoint has no model directory or repository the
			// platform knows: its own tokenizer is the one candidate.
			return benchrun.EngineTokenizerRef, nil
		}
		fallback := a.autoBenchmarkTokenizer(ctx, req)
		if req.Deployment == nil || req.Deployment.TokenizeDisabled {
			return fallback, nil
		}
		// The engine counts with its own tokenizer whatever format that is;
		// the copy or repository is what is loaded if it does not answer.
		return benchrun.EngineTokenizerRef + fallback, nil
	default:
		return "", productError(ErrorBenchmarkTokenizerSourceInvalid,
			fmt.Sprintf("unknown tokenizer source %q", req.Source), map[string]any{"source": req.Source})
	}
}

// repositoryTokenizerReference is the reference a repository source names:
// the client may send the repository as people write it ("org/model") or as
// a reference ("hf://org/model"); a reference for the other source is refused.
func repositoryTokenizerReference(source, reference string) (string, error) {
	scheme := "hf://"
	if source == benchmarkTokenizerSourceModelScope {
		scheme = "ms://"
	}
	reference = strings.TrimSpace(reference)
	if rest, ok := strings.CutPrefix(reference, scheme); ok {
		reference = rest
	} else if strings.Contains(reference, "://") {
		return "", benchmarkParameterError("tokenizer_ref", fmt.Sprintf("a %s tokenizer reference is a repository such as org/model or %sorg/model", source, scheme))
	}
	reference = strings.Trim(reference, "/")
	repo, _, _ := strings.Cut(reference, "@")
	if repo == "" || strings.Count(repo, "/") != 1 || strings.Contains(repo, "..") {
		return "", benchmarkParameterError("tokenizer_ref", fmt.Sprintf("a %s tokenizer reference is a repository such as org/model or %sorg/model", source, scheme))
	}
	return scheme + reference, nil
}

// autoBenchmarkTokenizer resolves the automatic source. The order is the point:
// a copy that is already on the machine that will read it is neither a download
// nor a guess about which repository this model came from, and the model
// repository is the fallback when there is no such copy. An empty result means
// neither exists. Token-length workloads refuse execution without a tokenizer;
// natural text workloads may use estimated local counts.
func (a *apiHandlers) autoBenchmarkTokenizer(ctx context.Context, req benchmarkTokenizerRequest) string {
	repository := ""
	if req.Deployment != nil {
		if model, err := a.d.Store.GetModel(req.Deployment.ModelID); err == nil {
			repository = tokenizerRefForModel(model)
			if declared := a.declaredBenchmarkTokenizer(req.Deployment, model); declared != "" {
				repository = declared
			}
		}
	}
	if req.LoadOrigin == "server" {
		// The server sends the load, so it cannot read a node's disk: the copy
		// on the target machine is pulled back over the data lane. Without a
		// lane — or without a copy — the repository reference stands, since the
		// node-local file is exactly what an isolated network makes unavailable.
		dir := a.nodeModelDir(req.Deployment, req.Node)
		if dir != "" && req.Node != nil {
			for _, name := range nodeTokenizerFiles {
				if fetched, err := a.fetchBenchmarkTokenizer(ctx, req.Node.UUID, filepath.Join(dir, name)); err == nil {
					return fetched
				}
			}
		}
		return repository
	}
	// A node load origin reads its own disk, so the candidate is that node's
	// copy — a multi-node deployment keeps its own path per node — and the
	// reference stays a path, so nothing crosses the network at creation time.
	origin, err := a.d.Store.NodeByUUID(req.LoadOrigin)
	if err != nil {
		return repository
	}
	if dir := a.nodeModelDir(req.Deployment, origin); dir != "" {
		for _, name := range nodeTokenizerFiles {
			if local := filepath.Join(dir, name); a.nodeTokenizerFileExists(ctx, req.LoadOrigin, local) {
				return local
			}
		}
	}
	return repository
}

// nodeTokenizerFiles are the vocabulary files a model directory may hold, in
// the order they are tried: tokenizer.json, then a tiktoken vocabulary.
var nodeTokenizerFiles = []string{"tokenizer.json", benchrun.TiktokenFile}

// nodeModelDir is the directory the target node already holds for the model
// this run measures, or "" when there is no such copy. The directory
// is the copy the node reads the deployment's model from
// (store.ModelSourceOn): the host path its instances mount, whichever storage
// holds it.
func (a *apiHandlers) nodeModelDir(deployment *store.Deployment, node *store.Node) string {
	if deployment == nil || node == nil || deployment.ModelID == 0 {
		return ""
	}
	rules, err := a.d.Store.SourceRulesFor(deployment)
	if err != nil {
		return ""
	}
	src, err := a.d.Store.ModelSourceOn(deployment.ModelID, node, rules)
	if err != nil || src.Action != store.SourceReady {
		return ""
	}
	dir := strings.TrimSpace(src.Location.HostPath)
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return dir
}

// nodeTokenizerFileExists confirms a candidate path is a regular file on the
// node. An agent that predates fs.stat cannot answer at all; that silence is
// not evidence against the path, which the platform itself assigned, so the
// candidate is adopted — and a run that cannot load it still reports the
// tokenizer failure before sending any inference requests.
func (a *apiHandlers) nodeTokenizerFileExists(ctx context.Context, nodeUUID, path string) bool {
	if a.d.CallNode == nil {
		return true
	}
	statCtx, cancel := context.WithTimeout(ctx, benchmarkTokenizerStatTimeout)
	defer cancel()
	raw, err := a.d.CallNode(statCtx, nodeUUID, proto.RPCFSStat, proto.FSStatParams{Path: path})
	if err != nil {
		return strings.Contains(strings.ToLower(err.Error()), "unknown rpc method: "+proto.RPCFSStat)
	}
	var stat proto.FSStatResult
	if err := json.Unmarshal(raw, &stat); err != nil {
		return false
	}
	return stat.Regular
}

// fetchBenchmarkTokenizer pulls one node-local file back to the server's
// artifact directory and returns its path. The copy is content-addressed, so
// repeated runs of the same model share one file.
func (a *apiHandlers) fetchBenchmarkTokenizer(ctx context.Context, nodeUUID, path string) (string, error) {
	if a.d.NodeStreams == nil {
		return "", errors.New("node file streams are unavailable")
	}
	dir := strings.TrimSpace(a.d.BenchmarkArtifactDir)
	if dir == "" {
		return "", errors.New("benchmark artifact directory unavailable")
	}
	conn, err := a.d.NodeStreams.OpenLargeFileRead(ctx, nodeUUID, path)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	raw, err := readBoundedNodeFile(ctx, conn)
	if err != nil {
		return "", err
	}
	// A file that is not a loadable tokenizer would put an agentic run into a
	// fatal failure at execution time; treating it as "no fetch" here keeps the
	// decision at creation, where the caller can still fall back.
	ext, ok := tokenizerFileKind(raw)
	if !ok {
		return "", errors.New("the node file is not a supported tokenizer.json or tiktoken vocabulary")
	}
	sum := sha256.Sum256(raw)
	target := filepath.Join(dir, fmt.Sprintf("%s%x%s", benchmarkTokenizerFetchedPrefix, sum[:8], ext))
	if _, err := os.Stat(target); err == nil {
		return target, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	// Write beside the target and rename: a concurrent create of the same model
	// resolves to the same name, and a reader must never see a partial file.
	tmp, err := os.CreateTemp(dir, "."+benchmarkTokenizerFetchedPrefix+"*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return "", err
	}
	return target, nil
}

// readBoundedNodeFile reads one file off a node stream, bounded by the same
// limit the agent enforces. A stream that fails, stalls or ends without a byte
// is an error: the caller has no way to tell an empty file from a refused one,
// and a tokenizer.json is never empty.
func readBoundedNodeFile(ctx context.Context, conn net.Conn) ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if deadline, ok := conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			// UTC because this package's clock reads all are; a read deadline is
			// an absolute instant either way.
			_ = deadline.SetReadDeadline(time.Now().UTC().Add(benchmarkTokenizerFetchIdle))
		}
		n, err := conn.Read(chunk)
		if n > 0 {
			if buf.Len()+n > proto.LargeFileReadMaxBytes {
				return nil, fmt.Errorf("file exceeds the %d byte limit", proto.LargeFileReadMaxBytes)
			}
			buf.Write(chunk[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if buf.Len() == 0 {
		return nil, errors.New("the node returned no bytes")
	}
	return buf.Bytes(), nil
}

// tokenizerFileKind reports whether raw is a tokenizer file the runtime can
// load, and the extension its stored copy takes: ".json" for a
// tokenizer.json, ".tiktoken" for a tiktoken vocabulary.
func tokenizerFileKind(raw []byte) (string, bool) {
	if validTokenizerJSON(raw) {
		return ".json", true
	}
	if benchrun.IsTiktokenVocabulary(raw) {
		return ".tiktoken", true
	}
	return "", false
}

// validTokenizerJSON reports whether raw is a tokenizer.json the runtime can
// load. The upload endpoint and the node fetch share it, so a fetched file
// cannot enter a run that the same content would have been refused for.
func validTokenizerJSON(raw []byte) bool {
	var probe struct {
		Model any `json:"model"`
		Vocab any `json:"vocab"`
		Added any `json:"added_tokens"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.Model != nil || probe.Vocab != nil || probe.Added != nil
}
