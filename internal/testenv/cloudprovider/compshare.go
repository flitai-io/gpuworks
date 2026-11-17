package cloudprovider

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	compsharedriver "gpuworks.ai/gpuworks/internal/server/provider/compshare"
)

// compshareEndpointPoll is how often Rent asks for the SSH endpoint of a
// new instance; tests shorten it.
var compshareEndpointPoll = 5 * time.Second

const (
	compshareRentTimeout    = 5 * time.Minute
	compshareDestroyTimeout = 2 * time.Minute
)

var compshareLedgerMu sync.Mutex

type compshareDriver interface {
	ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error)
	CreateInstance(context.Context, serverprovider.Credentials, serverprovider.CreateInstanceRequest) (serverprovider.Instance, error)
	DeleteInstance(context.Context, serverprovider.Credentials, string) error
	SSHAccess(context.Context, serverprovider.Credentials, string) (string, int, string, string, error)
}

type compshareCapacityChecker interface {
	CheckResourceCapacity(context.Context, serverprovider.Credentials, compsharedriver.ResourceCapacityRequest) ([]compsharedriver.ResourceCapacitySpec, error)
}

type compshareZoneLocator interface {
	SupportedZones(context.Context, serverprovider.Credentials) ([]serverprovider.Zone, error)
}

// CapacitySpec is a live CompShare capacity result used by acceptance tests.
type CapacitySpec struct {
	GPU            int
	CPU            int
	MemoryGB       int
	ResourceEnough bool
}

type Compshare struct {
	driver  compshareDriver
	creds   serverprovider.Credentials
	imageID string
	// containerImageID is the image of container instances, which are not
	// built from a VM image (see WithContainerImage).
	containerImageID string
	zoneImages       map[string]string
	runID            string
	mu               sync.Mutex
	placements       map[string]serverprovider.Zone
	// imageless holds region/zone pairs where creating a VM with this run's
	// image failed with RetCode 226641. The capacity API does not check the
	// image against the zone, so only a create reveals it; SelectPlacement
	// skips these zones afterwards.
	imageless map[string]bool
}

func NewCompshare(accessKey, secretKey, imageID, runID string) (*Compshare, error) {
	return NewCompshareZoneImages(accessKey, secretKey, imageID, nil, runID)
}

// NewCompshareZoneImages additionally maps regions to a region-local image.
// CompShare custom images are region-scoped and are not synced (cn-sh2-02
// reports DisableImageSync), so a cn-wlcb golden cannot boot a Shanghai VM
// (API error 226641); a region without an entry falls back to imageID.
func NewCompshareZoneImages(accessKey, secretKey, imageID string, zoneImages map[string]string, runID string) (*Compshare, error) {
	return newCompshare(compsharedriver.New(), serverprovider.Credentials{PublicKey: strings.TrimSpace(accessKey), PrivateKey: strings.TrimSpace(secretKey)}, imageID, zoneImages, runID)
}

func newCompshare(driver compshareDriver, creds serverprovider.Credentials, imageID string, zoneImages map[string]string, runID string) (*Compshare, error) {
	if creds.PublicKey == "" || creds.PrivateKey == "" {
		return nil, fmt.Errorf("compshare access key and secret key are required")
	}
	if strings.TrimSpace(runID) == "" {
		var err error
		runID, err = NewRunID()
		if err != nil {
			return nil, err
		}
	}
	return &Compshare{driver: driver, creds: creds, imageID: strings.TrimSpace(imageID), zoneImages: zoneImages, runID: runID, placements: make(map[string]serverprovider.Zone), imageless: make(map[string]bool)}, nil
}

// WithContainerImage sets the image container instances (GPUSpec.Container)
// boot from. CompShare's container product has its own image catalog, so the
// VM golden image cannot be used for it.
func (p *Compshare) WithContainerImage(imageID string) *Compshare {
	p.containerImageID = strings.TrimSpace(imageID)
	return p
}

func (p *Compshare) imageForZone(region string) string {
	if id, ok := p.zoneImages[strings.TrimSpace(region)]; ok && id != "" {
		return id
	}
	return p.imageID
}

func (p *Compshare) Name() string { return "compshare" }

func (p *Compshare) ManagementCredentials() serverprovider.Credentials { return p.creds }

// ListInstances exposes a sanitized provider inventory to L4 diagnostics.
// Credentials remain private to the adapter; callers only receive provider
// metadata such as IDs, addresses, and power state.
func (p *Compshare) ListInstances(ctx context.Context) ([]serverprovider.Instance, error) {
	return p.driver.ListInstances(ctx, p.creds)
}

// CheckCapacity performs an allocation-free inventory query for one GPU
// model. Keeping this on the test provider makes the real GPU matrix use the
// same credentials, image and signing path as Rent.
func (p *Compshare) CheckCapacity(ctx context.Context, gpuModel string, diskGB int) ([]CapacitySpec, error) {
	checker, ok := p.driver.(compshareCapacityChecker)
	if !ok {
		return nil, fmt.Errorf("compshare: driver does not support capacity checks")
	}
	rows, err := checker.CheckResourceCapacity(ctx, p.creds, compsharedriver.ResourceCapacityRequest{
		GPUModel: gpuModel, DiskGB: diskGB, Image: p.imageID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]CapacitySpec, 0, len(rows))
	for _, row := range rows {
		out = append(out, CapacitySpec{GPU: row.GPU, CPU: row.CPU, MemoryGB: row.MemoryGB, ResourceEnough: row.ResourceEnough})
	}
	return out, nil
}

// SelectPlacement finds a VM-capable zone and an available shape as one
// operation. The selected zone is retained for the following Rent call so the
// capacity check and create request cannot disagree about placement.
func (p *Compshare) SelectPlacement(ctx context.Context, gpuModel string, gpuCount, diskGB int) (CapacitySpec, serverprovider.Zone, error) {
	checker, ok := p.driver.(compshareCapacityChecker)
	if !ok {
		return CapacitySpec{}, serverprovider.Zone{}, fmt.Errorf("compshare: driver does not support capacity checks")
	}
	locator, ok := p.driver.(compshareZoneLocator)
	if !ok {
		return CapacitySpec{}, serverprovider.Zone{}, fmt.Errorf("compshare: driver does not support zone discovery")
	}
	zones, err := locator.SupportedZones(ctx, p.creds)
	if err != nil {
		return CapacitySpec{}, serverprovider.Zone{}, err
	}
	var zoneErrors []string
	queried := 0
	for _, zone := range zones {
		if zone.IsPod {
			continue
		}
		p.mu.Lock()
		skip := p.imageless[zoneKey(zone)]
		p.mu.Unlock()
		if skip {
			zoneErrors = append(zoneErrors, fmt.Sprintf("%s/%s: does not support image %s (RetCode 226641 at create)", zone.Region, zone.Zone, p.imageForZone(zone.Region)))
			continue
		}
		queried++
		rows, err := checker.CheckResourceCapacity(ctx, p.creds, compsharedriver.ResourceCapacityRequest{GPUModel: gpuModel, DiskGB: diskGB, Image: p.imageForZone(zone.Region), Region: zone.Region, Zone: zone.Zone})
		if err != nil {
			// A zone whose capacity query fails (image missing in that
			// region, provider-side error) must stay distinguishable from a
			// zone that answered "no capacity" — swallowing it silently is
			// how a half-usable region stays invisible in CI logs.
			zoneErrors = append(zoneErrors, fmt.Sprintf("%s/%s: %v", zone.Region, zone.Zone, err))
			continue
		}
		for _, row := range rows {
			if row.GPU == gpuCount && row.ResourceEnough {
				selected := CapacitySpec{GPU: row.GPU, CPU: row.CPU, MemoryGB: row.MemoryGB, ResourceEnough: row.ResourceEnough}
				p.mu.Lock()
				p.placements[placementKey(gpuModel, gpuCount)] = zone
				p.mu.Unlock()
				return selected, zone, nil
			}
		}
	}
	if len(zoneErrors) > 0 {
		return CapacitySpec{}, serverprovider.Zone{}, fmt.Errorf("no VM zone has %s x%d capacity (%d zone(s) answered; zone query errors: %s)", gpuModel, gpuCount, queried, strings.Join(zoneErrors, "; "))
	}
	return CapacitySpec{}, serverprovider.Zone{}, fmt.Errorf("no VM zone has %s x%d capacity", gpuModel, gpuCount)
}

func zoneKey(zone serverprovider.Zone) string { return zone.Region + "/" + zone.Zone }

// IsImageUnsupportedInZone reports CompShare's "The current availability zone
// does not support this image" create error.
func IsImageUnsupportedInZone(err error) bool {
	return errors.Is(err, compsharedriver.RetCode(226641))
}

// IsGPUTypeUnsupportedByImage reports CompShare's "gpu type is not support
// current image" capacity error: the card type exists, but not with the image
// this run uses.
func IsGPUTypeUnsupportedByImage(err error) bool {
	return errors.Is(err, compsharedriver.RetCode(226603))
}

// CompshareGPUDisplayName is the part of a CompShare GpuType that nvidia-smi
// prints: GpuType adds a memory variant after "_" ("4090_48G" is an
// "NVIDIA GeForce RTX 4090"), which the card name does not carry.
func CompshareGPUDisplayName(gpuType string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(gpuType), "_")
	return name
}

func placementKey(model string, count int) string {
	return strings.ToLower(strings.TrimSpace(model)) + "/" + strconv.Itoa(count)
}

func (p *Compshare) Rent(ctx context.Context, spec GPUSpec) (*Instance, error) {
	if spec.Container {
		if p.containerImageID == "" {
			return nil, fmt.Errorf("compshare container image id is required to rent a container instance")
		}
	} else if p.imageID == "" {
		return nil, fmt.Errorf("compshare image id is required for rent")
	}
	ctx, cancel := withMaxTimeout(ctx, compshareRentTimeout)
	defer cancel()
	password, err := generatePassword()
	if err != nil {
		return nil, err
	}
	gpuCount := spec.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	diskGB := spec.DiskGB
	if diskGB <= 0 {
		diskGB = 100
	}
	if spec.MinVRAMMB > 0 {
		known, ok := gpuVRAMMB(spec.GPUModel)
		if !ok {
			return nil, fmt.Errorf("compshare: cannot verify MinVRAMMB=%d for GPU %q", spec.MinVRAMMB, spec.GPUModel)
		}
		if known < spec.MinVRAMMB {
			return nil, fmt.Errorf("compshare: GPU %q has %d MiB VRAM, below requested %d MiB", spec.GPUModel, known, spec.MinVRAMMB)
		}
	}
	instanceKind := "vm"
	if spec.Container {
		instanceKind = "container"
	}
	extra := map[string]string{"disk_gb": strconv.Itoa(diskGB), "password": password, "instance_kind": instanceKind}
	if spec.Region != "" {
		extra["region"] = spec.Region
	}
	if spec.Zone != "" {
		extra["zone"] = spec.Zone
	}
	p.mu.Lock()
	placed, hasPlacement := p.placements[placementKey(spec.GPUModel, gpuCount)]
	p.mu.Unlock()
	// Container zones are not the VM zones SelectPlacement searches; with no
	// region and zone the driver looks among the container zones itself.
	hasPlacement = hasPlacement && spec.Region == "" && spec.Zone == "" && !spec.Container
	var created serverprovider.Instance
	var rejected []string
	for {
		if hasPlacement {
			extra["region"], extra["zone"] = placed.Region, placed.Zone
		}
		image := p.imageForZone(extra["region"])
		if spec.Container {
			image = p.containerImageID
		}
		created, err = p.driver.CreateInstance(ctx, p.creds, serverprovider.CreateInstanceRequest{Name: TestNamePrefix + p.runID, GPUModel: spec.GPUModel, GPUCount: gpuCount, Image: image, Extra: extra})
		if err == nil {
			break
		}
		// A placement chosen from capacity alone can land in a zone that does
		// not carry the image; exclude that zone and place again.
		if !hasPlacement || !IsImageUnsupportedInZone(err) {
			if len(rejected) > 0 {
				return nil, fmt.Errorf("%w (zones without the image: %s)", err, strings.Join(rejected, ", "))
			}
			return nil, err
		}
		p.mu.Lock()
		p.imageless[zoneKey(placed)] = true
		p.mu.Unlock()
		rejected = append(rejected, zoneKey(placed))
		if _, placed, err = p.SelectPlacement(ctx, spec.GPUModel, gpuCount, diskGB); err != nil {
			return nil, fmt.Errorf("compshare: no other zone for %s x%d after %s rejected image %s: %w", spec.GPUModel, gpuCount, strings.Join(rejected, ", "), image, err)
		}
	}
	if err := addCompshareLedger(created.ExternalID); err != nil {
		_ = p.driver.DeleteInstance(context.Background(), p.creds, created.ExternalID)
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), compshareDestroyTimeout)
			defer cleanupCancel()
			_ = p.Destroy(cleanupCtx, created.ExternalID)
		}
	}()
	instance := created
	for instance.PublicIP == "" || instance.SSHPort == 0 {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for compshare SSH endpoint: %w", ctx.Err())
		case <-time.After(compshareEndpointPoll):
		}
		instances, listErr := p.driver.ListInstances(ctx, p.creds)
		if listErr != nil {
			continue
		}
		for _, candidate := range instances {
			if candidate.ExternalID == created.ExternalID {
				instance = candidate
				break
			}
		}
	}
	host, port, user, actualPassword, err := p.driver.SSHAccess(ctx, p.creds, instance.ExternalID)
	if err != nil {
		return nil, fmt.Errorf("resolve compshare SSH access: %w", err)
	}
	if host != "" {
		instance.PublicIP = host
	}
	if port > 0 {
		instance.SSHPort = port
	}
	if user != "" {
		instance.SSHUser = user
	}
	if actualPassword != "" {
		password = actualPassword
	}
	failed = false
	return &Instance{
		InstanceID: instance.ExternalID, GPUModel: instance.GPUModel, GPUCount: instance.GPUCount,
		Kind: instance.Kind, Region: instance.Region, PublicIP: instance.PublicIP, PrivateIP: instance.PrivateIP,
		SSHPort: instance.SSHPort, Username: firstNonEmpty(instance.SSHUser, "root"), Password: password,
	}, nil
}

func (p *Compshare) Destroy(ctx context.Context, instanceID string) error {
	ctx, cancel := withMaxTimeout(ctx, compshareDestroyTimeout)
	defer cancel()
	err := p.driver.DeleteInstance(ctx, p.creds, instanceID)
	if err != nil && isTransientCompshareError(err) {
		select {
		case <-ctx.Done():
			return err
		case <-time.After(2 * time.Second):
		}
		err = p.driver.DeleteInstance(ctx, p.creds, instanceID)
	}
	if err != nil {
		return err
	}
	return removeCompshareLedger(instanceID)
}

// isTransientCompshareError reports a transport failure of a CompShare API
// call (connection torn down, refused, timed out) that a retry may get past.
func isTransientCompshareError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EAGAIN) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (p *Compshare) ListLeaked(ctx context.Context) ([]string, error) {
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
	ledger, ledgerErr := readCompshareLedger()
	if ledgerErr != nil {
		return nil, ledgerErr
	}
	for _, id := range ledger {
		if _, ok := seen[id]; !ok {
			leaked = append(leaked, id)
		}
	}
	return leaked, nil
}

func (p *Compshare) ListLeakedForRunID(ctx context.Context, runID string) ([]string, error) {
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return nil, err
	}
	return leakedInstanceIDsForRun(p.Name(), runID, instances)
}

func compshareLedgerPath() string { return filepath.Join(os.TempDir(), "gpuworks-test-instances.json") }

func readCompshareLedger() ([]string, error) {
	compshareLedgerMu.Lock()
	defer compshareLedgerMu.Unlock()
	return readCompshareLedgerLocked()
}

func readCompshareLedgerLocked() ([]string, error) {
	data, err := os.ReadFile(compshareLedgerPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read compshare test ledger: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("decode compshare test ledger: %w", err)
	}
	return ids, nil
}

func addCompshareLedger(id string) error {
	compshareLedgerMu.Lock()
	defer compshareLedgerMu.Unlock()
	ids, err := readCompshareLedgerLocked()
	if err != nil {
		return err
	}
	for _, existing := range ids {
		if existing == id {
			return nil
		}
	}
	return writeCompshareLedgerLocked(append(ids, id))
}

func removeCompshareLedger(id string) error {
	compshareLedgerMu.Lock()
	defer compshareLedgerMu.Unlock()
	ids, err := readCompshareLedgerLocked()
	if err != nil {
		return err
	}
	filtered := ids[:0]
	for _, existing := range ids {
		if existing != id {
			filtered = append(filtered, existing)
		}
	}
	return writeCompshareLedgerLocked(filtered)
}

func writeCompshareLedgerLocked(ids []string) error {
	path := compshareLedgerPath()
	if len(ids) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove compshare test ledger: %w", err)
		}
		return nil
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "gpuworks-test-instances-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write compshare test ledger: %w", err)
	}
	return nil
}

func generatePassword() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate compshare password: %w", err)
	}
	return "Gw1!" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
