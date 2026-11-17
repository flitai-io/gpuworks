// Package kvbackend is the catalog of KV offload backends (ADR 0069): which
// component keeps an instance's KV cache beyond the GPU, per engine. A row is
// a descriptor, not behaviour: the engine adapter in package deploy renders
// each row's engine configuration, and the gates it declares (vendors,
// topology, the PD roles that keep it) are enforced there.
//
// The catalog is compiled in. There is no registration at init and no
// mutable package state: Builtin builds the table inside a function, and a
// caller that needs another table (a test) builds its own Registry with New.
package kvbackend

import (
	"fmt"
	"slices"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// Native is the id of an engine's own KV offload (vLLM's CPU and disk tiers),
// the row an empty selection resolves to.
const Native = "native"

// Topology bounds the launches a backend runs in. MaxNNodes zero is
// unbounded; PP and DP say whether pipeline and data parallelism are allowed.
type Topology struct {
	MaxNNodes int
	PP, DP    bool
}

// Backend is one KV offload backend of one engine.
type Backend struct {
	// ID is unique per Engine; it is the value of the kv_offload_backend key.
	ID     string
	Engine string
	// Label is the backend's product name, shown as is.
	Label string
	// Default resolves an empty selection; at most one per engine, and it is
	// the engine's own offload, configured by the native dictionary keys.
	Default bool
	// Vendors is an allow-list of accelerator vendors; empty allows every
	// vendor the engine runs on.
	Vendors []string
	// External: the KV cache is kept by a component outside the engine
	// (RequestedFeatures.ExternalKVCache).
	External bool
	// Replaces are the native dictionary keys this backend excludes while it
	// is selected: a stated value for one of them is refused.
	Replaces []string
	// PDRoles are the PD roles that keep the backend, prefill always among
	// them; other roles launch without KV offload.
	PDRoles  []string
	Topology Topology
}

// KeptOn reports whether a PD instance of this role keeps the backend.
func (b Backend) KeptOn(role string) bool { return slices.Contains(b.PDRoles, role) }

// Supports reports whether the backend runs on the vendor. It is the one
// predicate both the deploy form's card list and the launch gate read.
func (b Backend) Supports(vendor string) bool {
	if len(b.Vendors) == 0 {
		return true
	}
	return slices.Contains(b.Vendors, accelerator.NormalizeVendor(vendor))
}

// TopologyAllows reports whether a launch of nNodes nodes, with or without
// pipeline and data parallelism, is one the backend runs in.
func (b Backend) TopologyAllows(nNodes int, pp, dp bool) bool {
	if b.Topology.MaxNNodes > 0 && nNodes > b.Topology.MaxNNodes {
		return false
	}
	return (b.Topology.PP || !pp) && (b.Topology.DP || !dp)
}

func (b Backend) clone() Backend {
	b.Vendors = slices.Clone(b.Vendors)
	b.Replaces = slices.Clone(b.Replaces)
	b.PDRoles = slices.Clone(b.PDRoles)
	return b
}

// Registry is an immutable, validated set of rows.
type Registry struct{ rows []Backend }

// Builtin is the catalog the product ships. Only the engine's own offload is
// in it today; a third-party backend joins as a row here plus a renderer in
// the engine adapter.
func Builtin() Registry {
	registry, err := New(Backend{
		ID: Native, Engine: engines.VLLM, Label: "vLLM", Default: true,
		PDRoles:  []string{pdmode.RolePrefill},
		Topology: Topology{PP: true, DP: true},
	})
	if err != nil {
		// The table is a literal; TestBuiltinIsValidAndHoldsOnlyTheNativeRow
		// keeps this unreachable.
		panic(err)
	}
	return registry
}

// New validates rows and returns them as a Registry, in the order given.
func New(rows ...Backend) (Registry, error) {
	defaults := map[string]bool{}
	seen := map[[2]string]bool{}
	out := make([]Backend, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" || row.Engine == "" || row.Label == "" {
			return Registry{}, fmt.Errorf("kv backend %q/%q: id, engine and label are required", row.Engine, row.ID)
		}
		if !slices.Contains(engines.DialectNames(), row.Engine) {
			return Registry{}, fmt.Errorf("kv backend %s/%s: engine has no launch dialect", row.Engine, row.ID)
		}
		key := [2]string{row.Engine, row.ID}
		if seen[key] {
			return Registry{}, fmt.Errorf("kv backend %s/%s: duplicate", row.Engine, row.ID)
		}
		seen[key] = true
		if row.Default {
			if row.External || len(row.Replaces) > 0 {
				return Registry{}, fmt.Errorf("kv backend %s/%s: the default row is the engine's own offload", row.Engine, row.ID)
			}
			if defaults[row.Engine] {
				return Registry{}, fmt.Errorf("kv backend %s/%s: a second default row", row.Engine, row.ID)
			}
			defaults[row.Engine] = true
		}
		for _, vendor := range row.Vendors {
			if !slices.Contains(accelerator.KnownVendors(), vendor) {
				return Registry{}, fmt.Errorf("kv backend %s/%s: vendor %q is not a canonical vendor", row.Engine, row.ID, vendor)
			}
		}
		for _, role := range row.PDRoles {
			if role != pdmode.RolePrefill && role != pdmode.RoleDecode {
				return Registry{}, fmt.Errorf("kv backend %s/%s: unknown PD role %q", row.Engine, row.ID, role)
			}
		}
		// The prefill computes the KV a cache tier serves again; a decoder
		// gets its prefix from the PD connector first, so a tier there only
		// writes. Every row is kept on prefill, which is also what lets the
		// PD warning say "prefill only" whenever decode does not keep it.
		if !slices.Contains(row.PDRoles, pdmode.RolePrefill) {
			return Registry{}, fmt.Errorf("kv backend %s/%s: PDRoles must include %s", row.Engine, row.ID, pdmode.RolePrefill)
		}
		if row.Topology.MaxNNodes < 0 {
			return Registry{}, fmt.Errorf("kv backend %s/%s: negative node bound", row.Engine, row.ID)
		}
		out = append(out, row.clone())
	}
	return Registry{rows: out}, nil
}

// Resolve answers the row an engine's selection names; an empty id is the
// engine's default row. ok is false for an id the engine does not have, and
// for an empty id on an engine with no default row.
func (r Registry) Resolve(engine, id string) (Backend, bool) {
	for _, row := range r.rows {
		if row.Engine == engine && (row.ID == id || id == "" && row.Default) {
			return row.clone(), true
		}
	}
	return Backend{}, false
}

// ForEngine lists the engine's rows in declaration order.
func (r Registry) ForEngine(engine string) []Backend {
	var out []Backend
	for _, row := range r.rows {
		if row.Engine == engine {
			out = append(out, row.clone())
		}
	}
	return out
}

// Selectable lists the rows a deployment selects by id: every row but the
// default, which an empty selection already names.
func (r Registry) Selectable(engine string) []Backend {
	var out []Backend
	for _, row := range r.ForEngine(engine) {
		if !row.Default {
			out = append(out, row)
		}
	}
	return out
}
