// system_labels.go generates the labels the platform itself puts on a node.
// They are not stored: every value here is a fact the server already holds
// (the node row, its cards, the resolved networks, the storages that apply to
// it), so writing them down
// would only create something to keep in sync with the probe. One function
// produces them for every reader - scheduling, evaluation and display - so a
// selector and the node page can never disagree about what a node carries.

package planning

import (
	"sort"
	"strconv"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SystemLabel is one generated (key, value) pair with the text to show for
// it. A key may appear more than once: the values of a key are a set, and a
// selector matches a node when the node carries any of the values it names.
//
// Display is always set: the English text to show when nothing translates
// the value. DisplayCode names an existing client translation for the few
// values that are platform vocabulary rather than someone's name - a vendor,
// the management network. A node's name, a network's name, a GPU model and a
// CPU architecture are shown as they are.
type SystemLabel struct {
	Key         string
	Value       string
	Display     string
	DisplayCode string
}

// IsNodeIdentity reports whether a system label is the node's own identity
// (its UUID), which every node carries a different value of.
func IsNodeIdentity(label SystemLabel) bool { return label.Key == nodelabel.KeyNode }

// SystemLabels is every label the platform puts on a node, in key then value
// order so two nodes with the same facts produce byte-identical output. A
// part of facts left nil leaves its labels off (NodeFacts).
func SystemLabels(node *store.Node, facts NodeFacts) []SystemLabel {
	if node == nil {
		return nil
	}
	labels := []SystemLabel{{Key: nodelabel.KeyNode, Value: node.UUID, Display: nodeDisplayName(node)}}
	if vendor := accelerator.NormalizeVendor(node.GPUVendor); vendor != "" {
		labels = append(labels, SystemLabel{
			Key: nodelabel.KeyVendor, Value: vendor,
			Display: accelerator.VendorBrand(vendor), DisplayCode: "common:vendor." + vendor,
		})
	}
	if node.Arch != "" {
		labels = append(labels, SystemLabel{Key: nodelabel.KeyCPUArch, Value: node.Arch, Display: node.Arch})
	}
	labels = append(labels, cardLabels(node)...)
	labels = append(labels, networkLabels(node, facts.Networks)...)
	labels = append(labels, storageLabels(node, facts.Storages)...)
	labels = append(labels, providerLabels(node, facts.Providers)...)
	labels = append(labels, kubeLabels(node, facts.KubeClusters)...)
	sortSystemLabels(labels)
	return labels
}

// cardLabels describes the node's cards. A node with two models carries both
// values, because a selector naming either one can still run there - on the
// matching cards. Unhealthy cards are described too: a card the scheduler
// will not pick is still a fact about the machine, and hiding it would make
// a node's labels flicker with its hardware health.
func cardLabels(node *store.Node) []SystemLabel {
	models, arches := map[string]bool{}, map[string]bool{}
	var labels []SystemLabel
	for _, gpu := range node.OwnedGPUs() {
		if name := accelerator.NormalizeGPUName(gpu.Name); name != "" && !models[name] {
			models[name] = true
			labels = append(labels, SystemLabel{Key: nodelabel.KeyGPUModel, Value: name, Display: name})
		}
		// An architecture this build does not recognize is left off rather
		// than guessed: a selector can still name the model.
		if family := node.ArchOf(gpu).Family; family != "" && !arches[family] {
			arches[family] = true
			labels = append(labels, SystemLabel{
				Key: nodelabel.KeyGPUArch, Value: family, Display: accelerator.ArchBrand(family),
			})
		}
	}
	return labels
}

// networkLabels names the network carrying each role. The role falls to the
// management network when nothing dedicated reaches it; a role the node
// turned off there, with no network of its own, carries no label at all,
// which is what makes "has a compute network" expressible as a selector.
func networkLabels(node *store.Node, resolved netplan.Resolution) []SystemLabel {
	if resolved == nil {
		return nil
	}
	keys := map[string]string{
		netplan.RoleCompute:     nodelabel.KeyComputeNetwork,
		netplan.RoleStorageSync: nodelabel.KeyStorageSyncNetwork,
		netplan.RoleServing:     nodelabel.KeyServingNetwork,
	}
	var labels []SystemLabel
	for _, role := range netplan.Roles {
		r, ok := resolved.For(node.ID, role)
		if !ok {
			continue
		}
		if r.IsManagement() {
			labels = append(labels, SystemLabel{
				Key: keys[role], Value: nodelabel.ManagementNetwork,
				Display: "Management network", DisplayCode: "networks:management.name",
			})
			continue
		}
		labels = append(labels, SystemLabel{
			Key:     keys[role],
			Value:   strconv.FormatUint(uint64(r.Network.ID), 10),
			Display: r.Network.Name,
		})
	}
	return labels
}

// providerLabels say where a rented node runs; a node added by hand, or one
// whose provider was not loaded, carries none.
func providerLabels(node *store.Node, providers map[uint]store.NodeProvider) []SystemLabel {
	provider, ok := providers[node.ID]
	if !ok {
		return nil
	}
	var labels []SystemLabel
	if provider.Kind != "" {
		labels = append(labels, SystemLabel{Key: nodelabel.KeyProvider, Value: provider.Kind, Display: provider.Kind})
	}
	if provider.Region != "" {
		labels = append(labels, SystemLabel{Key: nodelabel.KeyRegion, Value: provider.Region, Display: provider.Region})
	}
	return labels
}

func nodeDisplayName(node *store.Node) string {
	if node.Name != "" {
		return node.Name
	}
	return node.UUID
}

func sortSystemLabels(labels []SystemLabel) {
	sort.SliceStable(labels, func(i, j int) bool {
		return labelBefore(labels[i].Key, labels[i].Value, labels[j].Key, labels[j].Value)
	})
}

// labelBefore is the order labels are listed in: by key, then by value.
func labelBefore(keyA, valueA, keyB, valueB string) bool {
	if keyA != keyB {
		return keyA < keyB
	}
	return valueA < valueB
}

// SystemLabelValues is a node's values for one key, which is what a selector
// is matched against: it holds when the node carries any value the selector
// names.
func SystemLabelValues(labels []SystemLabel, key string) []string {
	var values []string
	for _, label := range labels {
		if label.Key == key {
			values = append(values, label.Value)
		}
	}
	return values
}
