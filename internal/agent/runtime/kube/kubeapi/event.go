package kubeapi

import (
	"context"
	"net/url"
)

// ObjectReference is what an Event is about (core/v1 ObjectReference, the
// fields the agent sets).
type ObjectReference struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
}

// EventSource is the component that reported an Event.
type EventSource struct {
	Component string `json:"component,omitempty"`
	Host      string `json:"host,omitempty"`
}

// Event is a core/v1 Event as the agent writes it: `kubectl describe` shows
// it under the involved object's Events.
type Event struct {
	APIVersion string          `json:"apiVersion,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Metadata   ObjectMeta      `json:"metadata"`
	Involved   ObjectReference `json:"involvedObject"`
	Reason     string          `json:"reason,omitempty"`
	Message    string          `json:"message,omitempty"`
	// Type is "Normal" or "Warning".
	Type string `json:"type,omitempty"`
	// FirstTimestamp and LastTimestamp are RFC 3339 times.
	FirstTimestamp     string      `json:"firstTimestamp,omitempty"`
	LastTimestamp      string      `json:"lastTimestamp,omitempty"`
	Count              int32       `json:"count,omitempty"`
	Source             EventSource `json:"source"`
	ReportingComponent string      `json:"reportingComponent,omitempty"`
	ReportingInstance  string      `json:"reportingInstance,omitempty"`
}

// EventsPath is the events collection of a namespace.
func EventsPath(ns string) string { return "/api/v1/namespaces/" + url.PathEscape(ns) + "/events" }

// CreateEvent creates ev in its own namespace (ev.Metadata.Namespace) and
// returns the stored event.
func (c *Client) CreateEvent(ctx context.Context, ev Event) (Event, error) {
	if ev.APIVersion == "" {
		ev.APIVersion = "v1"
	}
	if ev.Kind == "" {
		ev.Kind = "Event"
	}
	var out Event
	err := c.Create(ctx, EventsPath(ev.Metadata.Namespace), ev, &out)
	return out, err
}
