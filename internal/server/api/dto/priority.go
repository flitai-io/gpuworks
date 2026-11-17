package dto

import "time"

// PriorityClass is a request priority class (store.PriorityClass): a name
// keys and clients refer to and a value, larger is more important.
type PriorityClass struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Value       int    `json:"value"`
	Builtin     bool   `json:"builtin"`
	Default     bool   `json:"default"`
	// Deletable says whether DELETE would take it: not built in, not the
	// default, and named by no live key.
	Deletable bool      `json:"deletable"`
	CreatedAt time.Time `json:"created_at"`
}

// PriorityClasses is GET /priority-classes: the classes, most important
// first, with the bounds a value must keep and the policies a key may take
// (the console renders these; it decides none of them).
type PriorityClasses struct {
	Items    []PriorityClass `json:"items"`
	MinValue int             `json:"min_value"`
	MaxValue int             `json:"max_value"`
	Policies []string        `json:"policies"`
	// DefaultPolicy is the policy of a key that states none.
	DefaultPolicy string `json:"default_policy"`
	// CeilingPolicies are the policies a ceiling applies to; a binding of
	// another policy has none.
	CeilingPolicies []string `json:"ceiling_policies"`
}

// APIKeyPriority is an API key's priority binding: the class its requests
// run at ("" for the default class), the highest class a request may ask
// for ("" for its own class), and how a request may choose.
type APIKeyPriority struct {
	Class   string `json:"class"`
	Ceiling string `json:"ceiling"`
	Policy  string `json:"policy"`
}
