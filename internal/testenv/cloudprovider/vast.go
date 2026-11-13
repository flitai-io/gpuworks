package cloudprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	vastdriver "gpuworks.ai/gpuworks/internal/server/provider/vast"
	"gpuworks.ai/gpuworks/internal/testenv/sshutil"
)

const (
	vastRentTimeout       = 8 * time.Minute
	vastEndpointTimeout   = 2 * time.Minute
	vastSSHReadyTimeout   = 2 * time.Minute
	vastDestroyTimeout    = 2 * time.Minute
	vastDefaultMaxPrice   = 1.0
	vastDefaultOfferLimit = 30
	vastMinReliability    = 0.97
	vastMinInetDown       = 200
)

var (
	vastLedgerMu sync.Mutex
	vastGPUAllow = []string{
		"RTX 5070", "RTX 5070 Ti", "RTX 4080", "RTX 4080S", "RTX 4090",
		"RTX 3090", "RTX A4000", "RTX A5000", "A40", "L4",
	}
)

type vastLifecycleDriver interface {
	SearchOffers(context.Context, serverprovider.Credentials, vastdriver.SearchOffersRequest) ([]vastdriver.Offer, error)
	ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error)
	CreateInstance(context.Context, serverprovider.Credentials, serverprovider.CreateInstanceRequest) (serverprovider.Instance, error)
	DeleteInstance(context.Context, serverprovider.Credentials, string) error
}

type vastSSHAttacher interface {
	AttachSSHKey(context.Context, serverprovider.Credentials, string, string) error
}

// Vast rents low-cost, explicitly bounded Vast.ai instances for L4 tests.
type Vast struct {
	driver     vastLifecycleDriver
	creds      serverprovider.Credentials
	runID      string
	privateKey []byte
	maxPrice   float64
	image      string
	publicKey  string
	sshReady   func(context.Context, string, int, string, []byte) error
}

func NewVast(apiKey, runID string, privateKey []byte) (*Vast, error) {
	maxPrice := vastDefaultMaxPrice
	if raw := strings.TrimSpace(os.Getenv("VAST_MAX_HOURLY_PRICE")); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("vast: invalid VAST_MAX_HOURLY_PRICE %q", raw)
		}
		maxPrice = value
	}
	image := strings.TrimSpace(os.Getenv("VAST_TEST_IMAGE"))
	if image == "" {
		image = "vastai/base-image:cuda-12.8.1-auto"
	}
	p, err := newVast(vastdriver.New(), serverprovider.Credentials{APIKey: strings.TrimSpace(apiKey)}, runID, privateKey, maxPrice, image)
	if err != nil {
		return nil, err
	}
	p.sshReady = sshutil.WaitSSHReadyAs
	return p, nil
}

func newVast(driver vastLifecycleDriver, creds serverprovider.Credentials, runID string, privateKey []byte, maxPrice float64, image string) (*Vast, error) {
	if strings.TrimSpace(creds.APIKey) == "" {
		return nil, fmt.Errorf("vast API key is required")
	}
	if len(privateKey) == 0 {
		return nil, fmt.Errorf("vast SSH private key is required")
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("vast: parse SSH private key: %w", err)
	}
	publicKey := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	if strings.TrimSpace(runID) == "" {
		var err error
		runID, err = NewRunID()
		if err != nil {
			return nil, err
		}
	}
	if maxPrice <= 0 {
		return nil, fmt.Errorf("vast maximum hourly price must be positive")
	}
	if strings.TrimSpace(image) == "" {
		return nil, fmt.Errorf("vast test image is required")
	}
	return &Vast{
		driver: driver, creds: creds, runID: runID, privateKey: append([]byte(nil), privateKey...),
		publicKey: strings.TrimSpace(publicKey), maxPrice: maxPrice, image: strings.TrimSpace(image),
	}, nil
}

func (p *Vast) Name() string { return "vast" }

func (p *Vast) ManagementCredentials() serverprovider.Credentials { return p.creds }

func (p *Vast) Rent(ctx context.Context, spec GPUSpec) (*Instance, error) {
	ctx, cancel := withMaxTimeout(ctx, vastRentTimeout)
	defer cancel()
	gpuCount := spec.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	minVRAM := spec.MinVRAMMB
	if minVRAM <= 0 {
		minVRAM = 12000
	}
	diskGB := spec.DiskGB
	if diskGB <= 0 {
		diskGB = 20
	}
	offers, err := p.driver.SearchOffers(ctx, p.creds, vastdriver.SearchOffersRequest{
		GPUCount: gpuCount, MinGPURAMMB: minVRAM, MinReliability: vastMinReliability,
		MinInetDown: vastMinInetDown, DiskGB: diskGB, Limit: vastDefaultOfferLimit,
	})
	if err != nil {
		return nil, err
	}
	offers = p.acceptableOffers(offers, spec, gpuCount, minVRAM)
	if len(offers) == 0 {
		return nil, fmt.Errorf("vast: no allowlisted offer at or below $%.4f/hour for %d GPU(s) and %d MiB VRAM", p.maxPrice, gpuCount, minVRAM)
	}
	var createErrors []string
	for _, offer := range offers {
		created, createErr := p.driver.CreateInstance(ctx, p.creds, serverprovider.CreateInstanceRequest{
			Name: TestNamePrefix + p.runID, GPUModel: offer.GPUName, GPUCount: offer.NumGPUs, Image: p.image,
			Extra: map[string]string{"offer_id": offer.ID.String(), "disk_gb": strconv.Itoa(diskGB)},
		})
		if createErr != nil {
			createErrors = append(createErrors, fmt.Sprintf("offer %s: %v", offer.ID.String(), createErr))
			continue
		}
		if attacher, ok := p.driver.(vastSSHAttacher); ok {
			if err := attacher.AttachSSHKey(ctx, p.creds, created.ExternalID, p.publicKey); err != nil {
				_ = p.driver.DeleteInstance(context.Background(), p.creds, created.ExternalID)
				return nil, fmt.Errorf("vast: attach SSH key to instance %s: %w", created.ExternalID, err)
			}
		}
		instance, waitErr := p.finishRent(ctx, created, offer, minVRAM)
		if waitErr != nil {
			createErrors = append(createErrors, fmt.Sprintf("offer %s: %v", offer.ID.String(), waitErr))
			continue
		}
		if p.sshReady != nil {
			readyCtx, readyCancel := withMaxTimeout(ctx, vastSSHReadyTimeout)
			readyErr := p.sshReady(readyCtx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey)
			readyCancel()
			if readyErr != nil {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), vastDestroyTimeout)
				cleanupErr := p.Destroy(cleanupCtx, instance.InstanceID)
				cleanupCancel()
				if cleanupErr != nil {
					return nil, fmt.Errorf("vast: offer %s SSH readiness failed: %v; destroy instance %s: %w", offer.ID.String(), readyErr, instance.InstanceID, cleanupErr)
				}
				createErrors = append(createErrors, fmt.Sprintf("offer %s SSH readiness: %v", offer.ID.String(), readyErr))
				continue
			}
		}
		return instance, nil
	}
	return nil, fmt.Errorf("vast: all acceptable offers failed (%s)", strings.Join(createErrors, "; "))
}

func (p *Vast) acceptableOffers(offers []vastdriver.Offer, spec GPUSpec, gpuCount, minVRAM int) []vastdriver.Offer {
	wantModel := strings.TrimSpace(spec.GPUModel)
	out := offers[:0]
	for _, offer := range offers {
		if offer.ID.String() == "" || offer.NumGPUs != gpuCount || offer.GPURAMMB < minVRAM || offer.DPHTotal <= 0 || offer.DPHTotal > p.maxPrice {
			continue
		}
		if !vastAllowedGPU(offer.GPUName) {
			continue
		}
		if wantModel != "" && !strings.EqualFold(wantModel, "auto") && !sameGPUModel(wantModel, offer.GPUName) {
			continue
		}
		out = append(out, offer)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DPHTotal == out[j].DPHTotal {
			return out[i].Reliability > out[j].Reliability
		}
		return out[i].DPHTotal < out[j].DPHTotal
	})
	return out
}

func (p *Vast) finishRent(ctx context.Context, created serverprovider.Instance, offer vastdriver.Offer, minVRAM int) (*Instance, error) {
	if strings.TrimSpace(created.ExternalID) == "" {
		return nil, fmt.Errorf("vast: create returned an empty instance id")
	}
	if err := addVastLedger(created.ExternalID); err != nil {
		_ = p.driver.DeleteInstance(context.Background(), p.creds, created.ExternalID)
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), vastDestroyTimeout)
			defer cleanupCancel()
			_ = p.Destroy(cleanupCtx, created.ExternalID)
		}
	}()
	endpointCtx, endpointCancel := withMaxTimeout(ctx, vastEndpointTimeout)
	defer endpointCancel()
	observed := created
	var selected serverprovider.SSHConnection
	var lastErr error
	for selected.Host == "" || selected.Port == 0 {
		instances, err := p.driver.ListInstances(endpointCtx, p.creds)
		if err != nil {
			lastErr = err
		} else {
			lastErr = nil
			for _, candidate := range instances {
				if candidate.ExternalID == created.ExternalID {
					observed = candidate
					candidates, candidateErr := serverprovider.SSHInstallCandidates(candidate, "auto")
					if candidateErr != nil {
						lastErr = candidateErr
					} else if len(candidates) > 0 {
						selected = candidates[0]
					}
					break
				}
			}
		}
		if selected.Host != "" && selected.Port > 0 {
			break
		}
		select {
		case <-endpointCtx.Done():
			return nil, fmt.Errorf("wait for vast SSH endpoint: %w (instance=%s state=%s last_error=%v)", endpointCtx.Err(), created.ExternalID, observed.Power, lastErr)
		case <-time.After(5 * time.Second):
		}
	}
	if offer.GPURAMMB < minVRAM {
		return nil, fmt.Errorf("vast: selected offer has %d MiB VRAM, below requested %d MiB", offer.GPURAMMB, minVRAM)
	}
	failed = false
	return &Instance{
		InstanceID: observed.ExternalID, GPUModel: firstNonEmpty(observed.GPUModel, offer.GPUName),
		GPUCount: firstPositive(observed.GPUCount, offer.NumGPUs), Kind: "container", Region: firstNonEmpty(observed.Region, offer.Geolocation),
		PublicIP: selected.Host, SSHPort: selected.Port, Username: firstNonEmpty(selected.User, "root"),
		PrivateKey: append([]byte(nil), p.privateKey...),
	}, nil
}

// MappedPortHost is the address Vast maps the instance's container ports on
// (public_ipaddr), which a server-dialed agent is reached at. Rent's
// Instance.PublicIP is instead the host of the preferred SSH connection, and
// that may be one of Vast's SSH proxies (sshN.vast.ai) that holds none of the
// instance's other ports.
func (p *Vast) MappedPortHost(ctx context.Context, instanceID string) (string, error) {
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return "", fmt.Errorf("vast: list instances: %w", err)
	}
	for _, instance := range instances {
		if instance.ExternalID != instanceID {
			continue
		}
		if strings.TrimSpace(instance.PublicIP) == "" {
			return "", fmt.Errorf("vast: instance %s reports no public address", instanceID)
		}
		return instance.PublicIP, nil
	}
	return "", fmt.Errorf("vast: instance %s is not listed", instanceID)
}

func (p *Vast) Destroy(ctx context.Context, instanceID string) error {
	ctx, cancel := withMaxTimeout(ctx, vastDestroyTimeout)
	defer cancel()
	if err := p.driver.DeleteInstance(ctx, p.creds, instanceID); err != nil {
		return err
	}
	return removeVastLedger(instanceID)
}

func (p *Vast) ListLeaked(ctx context.Context) ([]string, error) {
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return nil, err
	}
	var leaked []string
	seen := map[string]struct{}{}
	for _, instance := range instances {
		if strings.HasPrefix(instance.Name, TestNamePrefix) {
			leaked = append(leaked, instance.ExternalID)
			seen[instance.ExternalID] = struct{}{}
		}
	}
	ledger, err := readVastLedger()
	if err != nil {
		return nil, err
	}
	for _, id := range ledger {
		if _, ok := seen[id]; !ok {
			leaked = append(leaked, id)
		}
	}
	return leaked, nil
}

func (p *Vast) ListLeakedForRunID(ctx context.Context, runID string) ([]string, error) {
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return nil, err
	}
	return leakedInstanceIDsForRun(p.Name(), runID, instances)
}

func vastAllowedGPU(model string) bool {
	for _, allowed := range vastGPUAllow {
		if sameGPUModel(allowed, model) {
			return true
		}
	}
	return false
}

func sameGPUModel(a, b string) bool {
	compact := func(value string) string {
		return strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "_", "", "NVIDIA", "", "GEFORCE", "").Replace(value))
	}
	return compact(a) == compact(b)
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func vastLedgerPath() string { return filepath.Join(os.TempDir(), "gpuworks-test-vast-instances.json") }

func readVastLedger() ([]string, error) {
	vastLedgerMu.Lock()
	defer vastLedgerMu.Unlock()
	return readVastLedgerLocked()
}

func readVastLedgerLocked() ([]string, error) {
	data, err := os.ReadFile(vastLedgerPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read vast test ledger: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("decode vast test ledger: %w", err)
	}
	return ids, nil
}

func addVastLedger(id string) error {
	vastLedgerMu.Lock()
	defer vastLedgerMu.Unlock()
	ids, err := readVastLedgerLocked()
	if err != nil {
		return err
	}
	for _, existing := range ids {
		if existing == id {
			return nil
		}
	}
	return writeVastLedgerLocked(append(ids, id))
}

func removeVastLedger(id string) error {
	vastLedgerMu.Lock()
	defer vastLedgerMu.Unlock()
	ids, err := readVastLedgerLocked()
	if err != nil {
		return err
	}
	filtered := ids[:0]
	for _, existing := range ids {
		if existing != id {
			filtered = append(filtered, existing)
		}
	}
	return writeVastLedgerLocked(filtered)
}

func writeVastLedgerLocked(ids []string) error {
	if len(ids) == 0 {
		if err := os.Remove(vastLedgerPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove vast test ledger: %w", err)
		}
		return nil
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	if err := os.WriteFile(vastLedgerPath(), data, 0o600); err != nil {
		return fmt.Errorf("write vast test ledger: %w", err)
	}
	return nil
}
