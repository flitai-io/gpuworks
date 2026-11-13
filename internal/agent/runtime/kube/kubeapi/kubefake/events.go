package kubefake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// Events returns copies of the events created in ns, in creation order.
func (s *Server) Events(ns string) []kubeapi.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []kubeapi.Event
	for _, ev := range s.events {
		if ev.Metadata.Namespace == ns {
			out = append(out, ev)
		}
	}
	return out
}

// invalidEvent is the first field of ev an API server would reject for a
// create in ns, or "": a DNS-subdomain name, an involved object that names
// a kind and a name and lives in the event's own namespace, and a type of
// Normal or Warning.
func invalidEvent(ev kubeapi.Event, ns string) string {
	switch {
	case !dns1123.MatchString(ev.Metadata.Name) || len(ev.Metadata.Name) > 253:
		return "metadata.name"
	case ev.Metadata.Namespace != "" && ev.Metadata.Namespace != ns:
		return "metadata.namespace"
	case ev.Involved.Kind == "" || ev.Involved.Name == "":
		return "involvedObject"
	case ev.Involved.Namespace != ns:
		return "involvedObject.namespace"
	case ev.Type != "Normal" && ev.Type != "Warning":
		return "type"
	}
	return ""
}

// serveEvents answers POST on a namespace's events. DenyEvents makes it a
// 403, as for a service account whose Role has no events rule.
func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request, ns string) {
	if r.Method != http.MethodPost {
		s.status(w, 405, "MethodNotAllowed", "method not allowed")
		return
	}
	s.mu.Lock()
	deny := s.DenyEvents
	s.mu.Unlock()
	if deny {
		s.status(w, 403, "Forbidden", fmt.Sprintf(`events is forbidden: User "system:serviceaccount:%s:gpuworks-agent" cannot create resource "events" in API group "" in the namespace %q`, ns, ns))
		return
	}
	var ev kubeapi.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		s.status(w, 400, "BadRequest", err.Error())
		return
	}
	if field := invalidEvent(ev, ns); field != "" {
		s.status(w, 422, "Invalid", fmt.Sprintf("Event %q is invalid: %s: Invalid value", ev.Metadata.Name, field))
		return
	}
	s.mu.Lock()
	for _, have := range s.events {
		if have.Metadata.Namespace == ns && have.Metadata.Name == ev.Metadata.Name {
			s.mu.Unlock()
			s.status(w, 409, "AlreadyExists", fmt.Sprintf("events %q already exists", ev.Metadata.Name))
			return
		}
	}
	s.rv++
	ev.Metadata.Namespace = ns
	ev.Metadata.UID = fmt.Sprintf("event-uid-%d", s.rv)
	ev.Metadata.ResourceVersion = strconv.Itoa(s.rv)
	s.events = append(s.events, ev)
	s.mu.Unlock()
	s.json(w, 201, ev)
}
