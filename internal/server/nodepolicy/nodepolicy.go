// Package nodepolicy is a node's placement policy: the per-node knobs an
// operator sets on how the scheduler may place work there. It is a leaf:
// the store embeds Policy in the node row, the scheduler reads it from the
// node snapshot, and the API's dictionary and patch are driven by Settings,
// so a new knob is one Policy field, one Setting and one migration column.
//
// It is not the node's scheduling state (cordon and drain, store
// NodeScheduling), which stops placement altogether.
package nodepolicy

import (
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Policy is a node's placement policy. Each field is one knob; its gorm
// column is placement_<key> on the node row (embedded with that prefix), NOT
// NULL with the knob's default, so a node that never set it reads the
// default.
type Policy struct {
	// ShareWithForeign lets the scheduler place a memory-limited dGPU
	// partition on a card processes outside the platform also use, when
	// memory fits and no card free of them fits (ADR 0068).
	ShareWithForeign bool `gorm:"column:share_with_foreign;not null;default:false"`
}

// Key names a knob on the wire, in the dictionary and in the UI's locale
// keys (nodes:placementPolicy.<key>.*). It equals the knob's column name
// without the placement_ prefix.
type Key string

// KeyShareWithForeign is Policy.ShareWithForeign.
const KeyShareWithForeign Key = "share_with_foreign"

// Kind is how a knob's value is written and shown.
type Kind string

// KindBool is an on/off knob.
const KindBool Kind = "bool"

// Facts are what the server knows about a node that decides whether a knob
// takes effect there (Applicability). The caller derives them from the node
// row it already holds.
type Facts struct {
	// PartitionBackendReady: a dGPU partition backend is ready on the node.
	PartitionBackendReady bool
	// TracksForeign: the node's agent keeps processes outside the platform
	// reported beside the platform's own instances.
	TracksForeign bool
}

// Setting describes one knob. Settings is the single source the dictionary
// route, the patch, the DTO and the store's write are driven by.
type Setting struct {
	Key     Key
	Kind    Kind
	Default any
	// Get reads the knob from a policy.
	Get func(Policy) any
	// Set validates a wire value by Kind and writes it into the policy.
	Set func(*Policy, json.RawMessage) error
	// Applies says whether the knob takes effect on a node with these
	// facts, and when not, the reason code (messages.NodePlacementPolicy*).
	Applies func(Facts) (ok bool, reason string)
}

// Reason codes Applicability reports.
const (
	ReasonNoPartitionBackend = messages.NodePlacementPolicyNoPartitionBackend
	ReasonAgentTooOld        = messages.NodePlacementPolicyAgentTooOld
)

// Settings lists every knob, in display order.
func Settings() []Setting {
	return []Setting{{
		Key: KeyShareWithForeign, Kind: KindBool, Default: false,
		Get: func(p Policy) any { return p.ShareWithForeign },
		Set: func(p *Policy, raw json.RawMessage) error { return setBool(&p.ShareWithForeign, raw) },
		Applies: func(f Facts) (bool, string) {
			switch {
			case !f.PartitionBackendReady:
				return false, ReasonNoPartitionBackend
			case !f.TracksForeign:
				return false, ReasonAgentTooOld
			}
			return true, ""
		},
	}}
}

// setBool writes a JSON boolean; anything else, null included, is refused.
func setBool(field *bool, raw json.RawMessage) error {
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return fmt.Errorf("want true or false")
	}
	*field = *value
	return nil
}

// Values is the policy as the wire shows it: every knob by key.
func Values(p Policy) map[Key]any {
	out := make(map[Key]any, len(Settings()))
	for _, setting := range Settings() {
		out[setting.Key] = setting.Get(p)
	}
	return out
}

// InvalidError is a patch the policy cannot take: an unknown key, or a value
// of the wrong kind for its knob.
type InvalidError struct {
	Key    Key
	Detail string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("placement policy %s: %s", e.Key, e.Detail)
}

// Apply writes patch into p, key by key, through each knob's Setting. A key
// it does not know or a value of the wrong kind refuses the whole patch.
func Apply(p Policy, patch map[Key]json.RawMessage) (Policy, error) {
	settings := map[Key]Setting{}
	for _, setting := range Settings() {
		settings[setting.Key] = setting
	}
	out := p
	for key, raw := range patch {
		setting, ok := settings[key]
		if !ok {
			return p, &InvalidError{Key: key, Detail: "unknown setting"}
		}
		if err := setting.Set(&out, raw); err != nil {
			return p, &InvalidError{Key: key, Detail: err.Error()}
		}
	}
	return out, nil
}

// Consent is what a placement request itself says about the cards: the
// zero value lets the scheduler choose.
type Consent struct {
	// CardsChosen: an operator picked the cards (planning: the "gpus" key
	// of a manual placement), which consents to sharing them.
	CardsChosen bool
}

// SharesForeign is whether a partition may be placed beside processes
// outside the platform on this node: the node allows it, or the operator
// chose the cards. It is the only statement of that rule.
func (p Policy) SharesForeign(c Consent) bool { return p.ShareWithForeign || c.CardsChosen }

// Availability is whether one knob takes effect on a node.
type Availability struct {
	Key        Key
	Applicable bool
	Reason     string
}

// Applicability says, for each knob, whether it takes effect on a node with
// these facts.
func Applicability(f Facts) []Availability {
	settings := Settings()
	out := make([]Availability, 0, len(settings))
	for _, setting := range settings {
		ok, reason := setting.Applies(f)
		out = append(out, Availability{Key: setting.Key, Applicable: ok, Reason: reason})
	}
	return out
}
