package cloudprovider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const giteeDefaultEndpoint = "https://ai.gitee.com/v1"

type giteeSpec struct {
	Zone      string `json:"zone"`
	GPUModel  string `json:"gpu_model"`
	GPUVRAMGB int    `json:"gpu_vram"`
}

type giteeCreateResult struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type giteeInstance struct {
	ID       string `json:"id"`
	GPUModel string `json:"gpu_model"`
	GPUCount int    `json:"gpu_num"`
	Status   string `json:"status"`
	SSH      struct {
		Host string `json:"ssh_host"`
		Port int    `json:"ssh_port"`
		User string `json:"ssh_user"`
	} `json:"ssh_props"`
}

// Gitee is the L4 adapter for the Gitee AI compute-market API. The public API
// has no resource-name/tag field, so cleanup is intentionally limited to IDs
// written to a local run-scoped ledger after a successful create response.
// It never discovers or deletes account-wide instances by heuristic.
type Gitee struct {
	token    string
	zone     string
	image    string
	runID    string
	endpoint string
	client   *http.Client
}

func NewGitee(token, zone, image, runID string) (*Gitee, error) {
	return newGitee(token, zone, image, runID, giteeDefaultEndpoint, &http.Client{Timeout: 30 * time.Second})
}

func newGitee(token, zone, image, runID, endpoint string, client *http.Client) (*Gitee, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("gitee API token is required")
	}
	if strings.TrimSpace(zone) == "" {
		return nil, fmt.Errorf("gitee compute zone is required")
	}
	if strings.TrimSpace(image) == "" {
		return nil, fmt.Errorf("gitee compute image is required")
	}
	if strings.TrimSpace(runID) == "" {
		var err error
		runID, err = NewRunID()
		if err != nil {
			return nil, err
		}
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Gitee{
		token: strings.TrimSpace(token), zone: strings.TrimSpace(zone), image: strings.TrimSpace(image),
		runID: strings.TrimSpace(runID), endpoint: strings.TrimRight(endpoint, "/"), client: client,
	}, nil
}

// giteeNoHostMarker is what the create call answers when every host matching
// the profile is rented out ("no qualifying available host found"). Gitee has
// no stock API: /compute/specs keeps listing sold-out models, so only a create
// reveals supply. A refused create produces no instance.
const giteeNoHostMarker = "没有找到符合条件的可用主机"

// ErrGiteeNoHost marks Gitee's create refusal for lack of an available host,
// the only Rent failure worth waiting out and retrying.
var ErrGiteeNoHost = errors.New("gitee: no available host matches the profile")

// IsGiteeNoHost reports whether err is Gitee's create refusal for lack of an
// available host.
func IsGiteeNoHost(err error) bool {
	return errors.Is(err, ErrGiteeNoHost)
}

// giteeNoHostError keeps the create failure's text and marks it
// ErrGiteeNoHost.
type giteeNoHostError struct{ err error }

func (e *giteeNoHostError) Error() string   { return e.err.Error() }
func (e *giteeNoHostError) Unwrap() []error { return []error{e.err, ErrGiteeNoHost} }

// giteeCreateFailure classifies a failed create where it was answered.
// External text: Gitee states "no qualifying available host" only in words,
// in an HTTP error body or a per-instance result.
func giteeCreateFailure(err error, answer string) error {
	if strings.Contains(answer, giteeNoHostMarker) {
		return &giteeNoHostError{err: err}
	}
	return err
}

func (p *Gitee) Name() string  { return "gitee" }
func (p *Gitee) RunID() string { return p.runID }

func (p *Gitee) Rent(ctx context.Context, spec GPUSpec) (*Instance, error) {
	gpuCount := spec.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	var specs []giteeSpec
	if err := p.get(ctx, "/compute/specs?zone="+url.QueryEscape(p.zone), &specs); err != nil {
		return nil, fmt.Errorf("gitee list compute specs: %w", err)
	}
	var selected *giteeSpec
	for i := range specs {
		if strings.EqualFold(strings.TrimSpace(specs[i].GPUModel), strings.TrimSpace(spec.GPUModel)) && strings.EqualFold(specs[i].Zone, p.zone) {
			selected = &specs[i]
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("gitee: GPU model %q is not available in zone %q", spec.GPUModel, p.zone)
	}
	if spec.MinVRAMMB > 0 && selected.GPUVRAMGB*1024 < spec.MinVRAMMB {
		return nil, fmt.Errorf("gitee: GPU %q has %d MiB VRAM, below requested %d MiB", selected.GPUModel, selected.GPUVRAMGB*1024, spec.MinVRAMMB)
	}
	password, err := giteePassword()
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"zone": p.zone, "gpu_model": selected.GPUModel, "gpu_num": gpuCount,
		"gpu_vram": selected.GPUVRAMGB, "image_ident": p.image, "password": password,
		"instance_count": 1, "require_same_host": true,
	}
	var created []giteeCreateResult
	if err := p.post(ctx, "/compute/instances", body, &created); err != nil {
		return nil, giteeCreateFailure(fmt.Errorf("gitee create compute instance: %w", err), err.Error())
	}
	if len(created) != 1 || !created[0].Success || strings.TrimSpace(created[0].ID) == "" {
		answer := fmt.Sprintf("%+v", created)
		return nil, giteeCreateFailure(fmt.Errorf("gitee create compute instance failed: %s", answer), answer)
	}
	id := strings.TrimSpace(created[0].ID)
	if err := addGiteeLedger(p.runID, id); err != nil {
		return nil, fmt.Errorf("record gitee instance %s: %w", id, err)
	}
	failed := true
	defer func() {
		if failed {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_ = p.Destroy(cleanupCtx, id)
		}
	}()

	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		instance, found, listErr := p.instance(ctx, id)
		if listErr == nil && found {
			if instance.Status == "failed" {
				return nil, fmt.Errorf("gitee instance %s entered failed state", id)
			}
			if instance.Status == "running" && instance.SSH.Host != "" && instance.SSH.Port > 0 && instance.SSH.User != "" {
				failed = false
				return &Instance{
					InstanceID: id, GPUModel: instance.GPUModel, GPUCount: instance.GPUCount,
					Kind: "gitee", Region: p.zone, PublicIP: instance.SSH.Host,
					SSHPort: instance.SSH.Port, Username: instance.SSH.User, Password: password,
					// Gitee instances always have a 60 GB data disk at /data. Route
					// the agent state there so layer blobs and decompressed tars fit
					// (the system disk is only 30 GB which the Huawei SWR vLLM image
					// alone would exhaust).
					DataDiskPath: "/data",
				}, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for gitee instance %s: %w", id, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	return nil, fmt.Errorf("wait for gitee instance %s SSH endpoint: timeout", id)
}

func (p *Gitee) Destroy(ctx context.Context, instanceID string) error {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return fmt.Errorf("gitee destroy requires an exact instance ID")
	}
	owned, err := giteeLedgerContains(p.runID, instanceID)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("gitee instance %s is not owned by test run %s", instanceID, p.runID)
	}
	_, found, err := p.instance(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("validate gitee instance %s before destroy: %w", instanceID, err)
	}
	if !found {
		return removeGiteeLedger(p.runID, instanceID)
	}
	var results []giteeCreateResult
	path := "/compute/instances/delete?ids=" + url.QueryEscape(instanceID)
	// DELETE is idempotent from the test's point of view, but the Gitee API can
	// accept the request and then close the connection before returning its
	// response.  A single POST therefore used to turn a successfully destroyed
	// instance into a red test.  Retry a bounded number of times and, after every
	// transport/response error, query the exact instance ID: if it has already
	// disappeared, the original request took effect and cleanup is successful.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("destroy gitee instance %s: %w", instanceID, ctx.Err())
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		results = nil
		err := p.post(ctx, path, nil, &results)
		if err == nil {
			if len(results) != 1 || results[0].ID != instanceID || !results[0].Success {
				lastErr = fmt.Errorf("returned %+v", results)
			} else {
				return removeGiteeLedger(p.runID, instanceID)
			}
		} else {
			lastErr = err
		}

		// A timeout may mean the server completed the deletion but the response
		// was lost.  Do not issue an unbounded account-wide cleanup; check only
		// the ledger-owned ID we were asked to destroy.
		if _, found, checkErr := p.instance(ctx, instanceID); checkErr == nil && !found {
			return removeGiteeLedger(p.runID, instanceID)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("destroy gitee instance %s: %w", instanceID, ctx.Err())
		}
	}
	return fmt.Errorf("destroy gitee instance %s after retries: %w", instanceID, lastErr)
}

func (p *Gitee) ListLeaked(ctx context.Context) ([]string, error) {
	ledger, err := readGiteeLedger()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ids := range ledger {
		out = append(out, ids...)
	}
	return p.existingIDs(ctx, out)
}

func (p *Gitee) ListLeakedForRunID(ctx context.Context, runID string) ([]string, error) {
	ledger, err := readGiteeLedger()
	if err != nil {
		return nil, err
	}
	return p.existingIDs(ctx, ledger[strings.TrimSpace(runID)])
}

func (p *Gitee) existingIDs(ctx context.Context, ids []string) ([]string, error) {
	var existing []string
	for _, id := range ids {
		if _, found, err := p.instance(ctx, id); err != nil {
			return nil, err
		} else if found {
			existing = append(existing, id)
		}
	}
	return existing, nil
}

func (p *Gitee) instance(ctx context.Context, id string) (giteeInstance, bool, error) {
	var instances []giteeInstance
	if err := p.get(ctx, "/compute/instances?ids="+url.QueryEscape(id), &instances); err != nil {
		return giteeInstance{}, false, err
	}
	for _, instance := range instances {
		if instance.ID == id {
			return instance, true, nil
		}
	}
	return giteeInstance{}, false, nil
}

func (p *Gitee) get(ctx context.Context, path string, out any) error {
	// GET is idempotent, so a transient network failure here is retried
	// rather than surfaced immediately. Destroy() calls this to validate
	// ownership before deleting a real billing instance; a single dropped
	// connection previously aborted the whole cleanup path and left the
	// instance running (real-machine confirmed 2026-08-28: "validate gitee
	// instance ... EOF" during Destroy skipped deletion entirely).
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		if lastErr = p.do(ctx, http.MethodGet, path, nil, out); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (p *Gitee) post(ctx context.Context, path string, body any, out any) error {
	return p.do(ctx, http.MethodPost, path, body, out)
}

func (p *Gitee) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.endpoint+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s status=%d body=%s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func giteePassword() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate gitee instance password: %w", err)
	}
	return "Gw1!" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

var giteeLedgerMu sync.Mutex

func giteeLedgerPath() string {
	return filepath.Join(os.TempDir(), "gpuworks-gitee-test-instances.json")
}

func readGiteeLedger() (map[string][]string, error) {
	giteeLedgerMu.Lock()
	defer giteeLedgerMu.Unlock()
	return readGiteeLedgerLocked()
}

func readGiteeLedgerLocked() (map[string][]string, error) {
	data, err := os.ReadFile(giteeLedgerPath())
	if os.IsNotExist(err) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read gitee test ledger: %w", err)
	}
	ledger := map[string][]string{}
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("decode gitee test ledger: %w", err)
	}
	return ledger, nil
}

func writeGiteeLedgerLocked(ledger map[string][]string) error {
	data, err := json.Marshal(ledger)
	if err != nil {
		return err
	}
	tmp := giteeLedgerPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, giteeLedgerPath())
}

func addGiteeLedger(runID, id string) error {
	giteeLedgerMu.Lock()
	defer giteeLedgerMu.Unlock()
	ledger, err := readGiteeLedgerLocked()
	if err != nil {
		return err
	}
	for _, existing := range ledger[runID] {
		if existing == id {
			return nil
		}
	}
	ledger[runID] = append(ledger[runID], id)
	return writeGiteeLedgerLocked(ledger)
}

func removeGiteeLedger(runID, id string) error {
	giteeLedgerMu.Lock()
	defer giteeLedgerMu.Unlock()
	ledger, err := readGiteeLedgerLocked()
	if err != nil {
		return err
	}
	filtered := ledger[runID][:0]
	for _, existing := range ledger[runID] {
		if existing != id {
			filtered = append(filtered, existing)
		}
	}
	if len(filtered) == 0 {
		delete(ledger, runID)
	} else {
		ledger[runID] = filtered
	}
	return writeGiteeLedgerLocked(ledger)
}

func giteeLedgerContains(runID, id string) (bool, error) {
	ledger, err := readGiteeLedger()
	if err != nil {
		return false, err
	}
	for _, existing := range ledger[runID] {
		if existing == id {
			return true, nil
		}
	}
	return false, nil
}
