// Package nodelabel is the vocabulary of node labels, shared by everything
// that writes, generates or selects on them. Two kinds exist:
//
//   - Written labels are the ones an operator puts on a node (and the label a
//     network attachment selects on). They may use any key outside the
//     reserved domain.
//   - System labels are the platform's own, generated from a node's facts.
//     Their keys are the fixed set declared here, all inside the reserved
//     domain, so the origin of a key is visible from the key alone.
//
// A deployment's node requirements name keys of either kind, which is why the
// two validators differ: a written label may never use the reserved domain,
// while a requirement may - but only to name a system label that exists.
//
// The package is a leaf with no dependency on store or planning: store
// validates what it persists with it, planning generates the system labels
// whose keys it declares, and neither may import the other.
package nodelabel

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Domain is the key domain reserved for system labels. A written label may
// not use it, nor any subdomain of it ("rack.gpuworks.ai/..."), so a
// generated label can never be shadowed by a written one.
const Domain = "gpuworks.ai"

// Prefix is Domain as it appears at the head of a system label key.
const Prefix = Domain + "/"

// The system label keys. Each is a question a placement asks about a node:
// which machine, what hardware, which networks.
const (
	// KeyNode is the node's own identity (its UUID), which is how a manual
	// placement is expressed: node ∈ {…}.
	KeyNode = Prefix + "node"
	// KeyVendor is the accelerator vendor, the first filter on any candidate
	// pool: a deployment carries one image, so it runs on one vendor.
	KeyVendor = Prefix + "vendor"
	// KeyCPUArch is the host CPU architecture, which decides which image
	// builds can run at all.
	KeyCPUArch = Prefix + "cpu-arch"
	// KeyGPUModel and KeyGPUArch describe the cards. Both are per-card
	// facts, so a mixed node carries several values.
	KeyGPUModel = Prefix + "gpu-model"
	KeyGPUArch  = Prefix + "gpu-arch"
	// The network keys name the network carrying each role, by id.
	KeyComputeNetwork     = Prefix + "compute-network"
	KeyStorageSyncNetwork = Prefix + "storage-sync-network"
	KeyServingNetwork     = Prefix + "serving-network"
	// KeyStorage names each storage that applies to the node, by id. A
	// deployment that must read its model from particular storages says so
	// with it; without it a node reads whichever copy it has.
	KeyStorage = Prefix + "storage"
	// KeyProvider and KeyRegion say where a node rented through a cloud
	// account runs: the provider (runpod, vast, autodl, compshare...) and its
	// region as the provider names it. A node added by hand has neither.
	KeyProvider = Prefix + "provider"
	KeyRegion   = Prefix + "region"
	// KeyKubernetesCluster and KeyKubernetesNode say where a node that joined
	// from a Kubernetes cluster sits: the cluster (by its UUID, which a rename
	// does not change) and the node's name in it. A node added any other way
	// has neither.
	KeyKubernetesCluster = Prefix + "kubernetes-cluster"
	KeyKubernetesNode    = Prefix + "kubernetes-node"
)

// ManagementNetwork is the value a network key takes when the management
// network carries that role rather than a declared network. It is not a
// network id: the management network is not a row, and a requirement that
// asks for it is asking for "nothing dedicated", which is a real choice.
const ManagementNetwork = "management"

// SystemKeys is every system label key, in a stable order.
func SystemKeys() []string {
	return []string{
		KeyNode, KeyVendor, KeyCPUArch, KeyGPUModel, KeyGPUArch,
		KeyComputeNetwork, KeyStorageSyncNetwork, KeyServingNetwork, KeyStorage,
		KeyProvider, KeyRegion, KeyKubernetesCluster, KeyKubernetesNode,
	}
}

// IsSystemKey reports whether key is one of the system label keys, exactly
// as declared.
func IsSystemKey(key string) bool {
	for _, system := range SystemKeys() {
		if key == system {
			return true
		}
	}
	return false
}

// Reserved reports whether a key sits in the reserved domain: the domain
// itself or a subdomain of it, matched without case so a key cannot slip
// through in different letters.
func Reserved(key string) bool {
	domain, _, ok := strings.Cut(key, "/")
	if !ok {
		return false
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	return domain == Domain || strings.HasSuffix(domain, "."+Domain)
}

// MaxKeys bounds a label map and a requirement set alike.
const MaxKeys = 64

const maxValueLength = 256

var keyGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)

func validKey(key string) error {
	if !keyGrammar.MatchString(key) {
		return fmt.Errorf("invalid node label key %q", key)
	}
	return nil
}

func validValue(key, value string) error {
	if len(value) > maxValueLength || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("invalid value for node label %q", key)
	}
	return nil
}

// ValidateWritten checks a written label map: a node's labels, or the one
// pair a network attachment selects on. Empty values are meaningful; a
// missing key is not an empty value.
func ValidateWritten(labels map[string]string) error {
	if len(labels) > MaxKeys {
		return fmt.Errorf("node labels must contain at most %d entries", MaxKeys)
	}
	for key, value := range labels {
		if err := validKey(key); err != nil {
			return err
		}
		if Reserved(key) {
			return fmt.Errorf("node label key %q uses the reserved %s namespace", key, Domain)
		}
		if err := validValue(key, value); err != nil {
			return err
		}
	}
	return nil
}
