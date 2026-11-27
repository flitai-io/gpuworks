// Package priority decides a gateway request's priority class: the classes
// an administrator defines (a name and a value, larger is more important),
// what an API key is bound to, and what the client asked for. It is a leaf:
// the store holds the classes and bindings, the gateway applies the result,
// and this package only rules (docs/explanation/request-priority.md).
package priority

import (
	"regexp"
	"sort"
	"strings"
)

// Value bounds of a class. The engines accept far wider ranges; the platform
// keeps a small one so a class value is never one an engine mishandles
// (SGLang 0.5.21 crashes on 2^63-1) and stays readable.
const (
	MinValue = -100
	MaxValue = 100
)

// Built-in class names. Their values are fixed; only display names and
// descriptions may change, and they cannot be deleted.
const (
	Critical = "critical"
	High     = "high"
	Normal   = "normal"
	Low      = "low"
	Batch    = "batch"
)

// Builtin is a built-in class and its value, in display order. The gap of
// 20 exceeds SGLang's default preemption threshold (10, a difference
// strictly greater preempts), so a request one class higher preempts.
type Builtin struct {
	Name  string
	Value int
}

// Builtins are the classes every installation starts with; Normal is the
// initial global default and Batch the class batch requests always run at.
var Builtins = []Builtin{{Critical, 40}, {High, 20}, {Normal, 0}, {Low, -20}, {Batch, -40}}

// IsBuiltin reports whether name is a built-in class.
func IsBuiltin(name string) bool {
	for _, b := range Builtins {
		if b.Name == name {
			return true
		}
	}
	return false
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidName reports whether name may name a class: lowercase letters, digits
// and hyphens, starting with a letter, at most 32 characters. Clients send it
// in a header, so it stays a plain token.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// ValidValue reports whether value is within the class value bounds.
func ValidValue(value int) bool { return value >= MinValue && value <= MaxValue }

// Policy is how a key's requests may choose their class.
type Policy string

const (
	// PolicyFixed: every request runs at the key's class; a requested class
	// that exists has no effect, and one that does not is refused as under
	// every policy.
	PolicyFixed Policy = "fixed"
	// PolicyWithinCeiling: a request may name any class whose value does not
	// exceed the key's ceiling class (its own class when it has none); one
	// above is lowered to the ceiling. The default.
	PolicyWithinCeiling Policy = "within_ceiling"
	// PolicyDownOnly: a request may lower itself below the key's class but
	// never raise above it.
	PolicyDownOnly Policy = "down_only"
)

// Policies are the policies in display order.
var Policies = []Policy{PolicyWithinCeiling, PolicyFixed, PolicyDownOnly}

// DefaultPolicy is the policy of a key that states none.
const DefaultPolicy = PolicyWithinCeiling

// ParsePolicy reads a stored or requested policy; empty is the default.
func ParsePolicy(text string) (Policy, bool) {
	switch Policy(strings.TrimSpace(text)) {
	case "":
		return DefaultPolicy, true
	case PolicyFixed:
		return PolicyFixed, true
	case PolicyWithinCeiling:
		return PolicyWithinCeiling, true
	case PolicyDownOnly:
		return PolicyDownOnly, true
	}
	return "", false
}

// Class is a priority class as the rules see it.
type Class struct {
	ID    uint
	Name  string
	Value int
}

// Table is the classes in force: every class by ID and by name, and the
// global default (the class of a request whose key names none, or that has
// no key).
type Table struct {
	byID    map[uint]Class
	byName  map[string]Class
	Default Class
}

// NewTable indexes classes; defaultID names the global default. A missing
// default falls back to the Normal class, and without it to value 0, so a
// table is always usable.
func NewTable(classes []Class, defaultID uint) Table {
	t := Table{byID: make(map[uint]Class, len(classes)), byName: make(map[string]Class, len(classes))}
	for _, c := range classes {
		t.byID[c.ID] = c
		t.byName[c.Name] = c
	}
	if c, ok := t.byID[defaultID]; ok {
		t.Default = c
	} else if c, ok := t.byName[Normal]; ok {
		t.Default = c
	} else {
		t.Default = Class{Name: Normal}
	}
	return t
}

// ByName is the class named name.
func (t Table) ByName(name string) (Class, bool) {
	c, ok := t.byName[name]
	return c, ok
}

// byIDOr is the class with id, or fallback when id is 0 or unknown (a class
// deleted while a key still named it cannot happen through the store, but a
// stale cache may lag a moment).
func (t Table) byIDOr(id uint, fallback Class) Class {
	if c, ok := t.byID[id]; ok && id != 0 {
		return c
	}
	return fallback
}

// Binding is what an API key says about its requests' priority.
type Binding struct {
	ClassID   uint // 0: the global default
	CeilingID uint // 0: the key's own class
	Policy    Policy
}

// Source is where a request came from, as far as priority cares.
type Source int

const (
	// SourceKey is a request authenticated by an API key.
	SourceKey Source = iota
	// SourceAnonymous is a request with no key (the playground, open access,
	// the server's own probes): it runs at the global default.
	SourceAnonymous
	// SourceBatch is a batch job's request: the class the batch was created
	// with, the Batch class when it named none or that class is gone.
	SourceBatch
	// SourceConsole is a request a signed-in console user sends through the
	// playground (a server-origin evaluation included): the class it names,
	// any class, or the default when it names none. Every console user is an
	// administrator, who may bind a key to any class anyway.
	SourceConsole
)

// Decision is the class a request runs at, and whether the class it asked
// for was lowered to fit its key.
type Decision struct {
	Class   Class
	Lowered bool
}

// Resolve decides a request's class. requested is the class name the client
// asked for ("" for none); ok is false when it names no class, which the
// gateway refuses rather than guessing.
func Resolve(t Table, source Source, binding Binding, requested string) (Decision, bool) {
	// Class names are lowercase, so a client's case does not matter.
	requested = strings.ToLower(strings.TrimSpace(requested))
	switch source {
	case SourceBatch:
		if c, found := t.ByName(requested); found && requested != "" {
			return Decision{Class: c}, true
		}
		if c, found := t.ByName(Batch); found {
			return Decision{Class: c}, true
		}
		return Decision{Class: t.Default}, true
	case SourceAnonymous:
		return Decision{Class: t.Default}, true
	case SourceConsole:
		if requested == "" {
			return Decision{Class: t.Default}, true
		}
		c, found := t.ByName(requested)
		return Decision{Class: c}, found
	}
	own, ceiling := t.keyBounds(binding)
	if requested == "" {
		return Decision{Class: own}, true
	}
	asked, found := t.ByName(requested)
	if !found {
		return Decision{}, false
	}
	if binding.Policy == PolicyFixed {
		return Decision{Class: own}, true
	}
	if asked.Value > ceiling.Value {
		return Decision{Class: ceiling, Lowered: true}, true
	}
	return Decision{Class: asked}, true
}

// keyBounds is a key's own class (what its requests run at when they name
// none) and its ceiling (what a request naming a higher class runs at).
// Only within_ceiling has a ceiling of its own; under fixed and down_only it
// is the key's own class.
func (t Table) keyBounds(binding Binding) (own, ceiling Class) {
	own = t.byIDOr(binding.ClassID, t.Default)
	if binding.Policy == PolicyFixed || binding.Policy == PolicyDownOnly {
		return own, own
	}
	ceiling = t.byIDOr(binding.CeilingID, own)
	if ceiling.Value < own.Value {
		// A ceiling below the key's own class never lowers the class it
		// runs at by default.
		ceiling = own
	}
	return own, ceiling
}

// Reach is what a key's requests can run at, for a client that wants to
// know before it sends: Own without a class header, Reachable every class
// a header can run at (Own included, highest value first, then by name),
// and Ceiling what a header naming any other class runs at.
type Reach struct {
	Own       Class
	Ceiling   Class
	Reachable []Class
}

// KeyReach is the reach of a key's binding, by the rules Resolve applies: a
// class is reachable exactly when a request naming it runs at it.
func KeyReach(t Table, binding Binding) Reach {
	own, ceiling := t.keyBounds(binding)
	reach := Reach{Own: own, Ceiling: ceiling}
	hasOwn := false
	for name, class := range t.byName {
		if decision, ok := Resolve(t, SourceKey, binding, name); ok && decision.Class == class {
			reach.Reachable = append(reach.Reachable, class)
			hasOwn = hasOwn || class == own
		}
	}
	if !hasOwn {
		// The neutral default of a table without classes.
		reach.Reachable = append(reach.Reachable, own)
	}
	sort.Slice(reach.Reachable, func(i, j int) bool {
		a, b := reach.Reachable[i], reach.Reachable[j]
		if a.Value != b.Value {
			return a.Value > b.Value
		}
		return a.Name < b.Name
	})
	return reach
}
