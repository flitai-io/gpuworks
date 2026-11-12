package kubeapply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// FieldManager owns the fields the server applies (server-side apply).
const FieldManager = "gpuworks"

// Object is one object of a manifest.
type Object struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	body      map[string]any
}

// Actions an object's result reports.
const (
	ActionCreate    = "create"    // the object did not exist and is (or would be) created
	ActionUpdate    = "update"    // the object exists and is (or would be) brought to the manifest
	ActionDelete    = "delete"    // the object is (or would be) deleted
	ActionReplace   = "replace"   // a field that cannot change did: deleted and created again
	ActionAbsent    = "absent"    // nothing to delete
	ActionFailed    = "failed"    // the API server refused; Result.Error says why
	ActionNotTried  = "not_tried" // an earlier object failed
	ActionDependent = "dependent" // a dry run cannot check it before its namespace exists
)

// Result is what happened to one object.
type Result struct {
	Object
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// resources maps the kinds a join manifest holds to their API paths. Any
// other kind is refused: this package applies gpuworks' own manifest only.
var resources = map[string]struct {
	group, resource string
	namespaced      bool
}{
	"Namespace":          {"/api/v1", "namespaces", false},
	"ServiceAccount":     {"/api/v1", "serviceaccounts", true},
	"Secret":             {"/api/v1", "secrets", true},
	"Role":               {"/apis/rbac.authorization.k8s.io/v1", "roles", true},
	"RoleBinding":        {"/apis/rbac.authorization.k8s.io/v1", "rolebindings", true},
	"ClusterRole":        {"/apis/rbac.authorization.k8s.io/v1", "clusterroles", false},
	"ClusterRoleBinding": {"/apis/rbac.authorization.k8s.io/v1", "clusterrolebindings", false},
	"PriorityClass":      {"/apis/scheduling.k8s.io/v1", "priorityclasses", false},
	"DaemonSet":          {"/apis/apps/v1", "daemonsets", true},
}

func (o Object) path() string {
	r := resources[o.Kind]
	if r.namespaced {
		return r.group + "/namespaces/" + url.PathEscape(o.Namespace) + "/" + r.resource + "/" + url.PathEscape(o.Name)
	}
	return r.group + "/" + r.resource + "/" + url.PathEscape(o.Name)
}

// Objects splits a multi-document manifest into its objects, in order.
func Objects(manifest string) ([]Object, error) {
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	var out []Object
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
		if doc == nil {
			continue
		}
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		r, ok := resources[kind]
		switch {
		case !ok:
			return nil, fmt.Errorf("manifest: kind %q is not one this package applies", kind)
		case name == "":
			return nil, fmt.Errorf("manifest: a %s has no name", kind)
		case r.namespaced && ns == "":
			return nil, fmt.Errorf("manifest: %s %s has no namespace", kind, name)
		}
		out = append(out, Object{Kind: kind, Name: name, Namespace: ns, body: doc})
	}
}

// APIError is a refusal from the API server, with its Status message.
type APIError struct {
	Code    int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Code)
	}
	return fmt.Sprintf("HTTP %d %s", e.Code, e.Reason)
}

func isNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, contentType string, body []byte) ([]byte, error) {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API server %s: %w", c.base.Host, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		apiErr := &APIError{Code: resp.StatusCode, Reason: http.StatusText(resp.StatusCode)}
		var status struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal(out, &status) == nil {
			apiErr.Message = status.Message
			if status.Reason != "" {
				apiErr.Reason = status.Reason
			}
		}
		return nil, apiErr
	}
	return out, nil
}

func (c *Client) exists(ctx context.Context, o Object) (bool, error) {
	_, found, err := c.current(ctx, o)
	return found, err
}

// current reads the object as the API server has it.
func (c *Client) current(ctx context.Context, o Object) (map[string]any, bool, error) {
	raw, err := c.do(ctx, http.MethodGet, o.path(), nil, "", nil)
	if isNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, true, err
	}
	return doc, true, nil
}

// needsReplace says whether have cannot be brought to want in place: a
// PriorityClass's value and preemption policy cannot change once it exists
// (an entry's priority or preemption was edited).
func needsReplace(o Object, have map[string]any) bool {
	if o.Kind != "PriorityClass" || have == nil {
		return false
	}
	// The manifest's numbers come from YAML (ints), the API server's from
	// JSON (float64s).
	num := func(v any) float64 {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case int64:
			return float64(n)
		case uint64:
			return float64(n)
		}
		return 0
	}
	policy := func(v any) string {
		if s, _ := v.(string); s != "" {
			return s
		}
		return "PreemptLowerPriority"
	}
	return num(have["value"]) != num(o.body["value"]) || policy(have["preemptionPolicy"]) != policy(o.body["preemptionPolicy"])
}

// Apply brings each object to the manifest with server-side apply, in
// order, and stops at the first refusal (later objects depend on earlier
// ones). A dry run asks the API server to check every change without
// making it; objects inside a namespace the manifest itself creates cannot
// be checked before it exists and are reported as dependent.
func (c *Client) Apply(ctx context.Context, objs []Object, dryRun bool) ([]Result, error) {
	results := make([]Result, 0, len(objs))
	newNamespaces := map[string]bool{}
	var failed error
	for _, o := range objs {
		if failed != nil {
			results = append(results, Result{Object: o, Action: ActionNotTried})
			continue
		}
		have, found, err := c.current(ctx, o)
		if err != nil {
			failed = err
			results = append(results, Result{Object: o, Action: ActionFailed, Error: err.Error()})
			continue
		}
		action := ActionUpdate
		if !found {
			action = ActionCreate
		}
		if needsReplace(o, have) {
			action = ActionReplace
			if !dryRun {
				// Pods that run under it keep their priority; new ones take the
				// new value once it is created again below.
				if _, err := c.do(ctx, http.MethodDelete, o.path(), nil, "application/json", nil); err != nil && !isNotFound(err) {
					failed = err
					results = append(results, Result{Object: o, Action: ActionFailed, Error: err.Error()})
					continue
				}
			} else {
				// The API server would refuse the changed fields: report the
				// replacement without asking it to check an update.
				results = append(results, Result{Object: o, Action: action})
				continue
			}
		}
		if dryRun && newNamespaces[o.Namespace] {
			results = append(results, Result{Object: o, Action: ActionDependent})
			continue
		}
		if dryRun && o.Kind == "Namespace" && !found {
			newNamespaces[o.Name] = true
		}
		body, err := json.Marshal(o.body)
		if err != nil {
			return results, err
		}
		q := url.Values{"fieldManager": {FieldManager}, "force": {"true"}}
		if dryRun {
			q.Set("dryRun", "All")
		}
		if _, err := c.do(ctx, http.MethodPatch, o.path(), q, "application/apply-patch+yaml", body); err != nil {
			failed = err
			results = append(results, Result{Object: o, Action: ActionFailed, Error: err.Error()})
			continue
		}
		results = append(results, Result{Object: o, Action: action})
	}
	return results, failed
}

// Delete removes the manifest's objects: the DaemonSet first (its pods,
// instance and hold pods alike, go with it), then everything else in
// reverse order. Its cluster-wide objects are named for its namespace
// (ClusterRole, PriorityClass), so another gpuworks namespace of the
// cluster keeps its own. A dry run reports what would happen. It continues past
// a refusal, so one forbidden object does not leave the rest behind, and
// returns the first refusal.
func (c *Client) Delete(ctx context.Context, objs []Object, dryRun bool) ([]Result, error) {
	ordered := make([]Object, 0, len(objs))
	for _, o := range objs {
		if o.Kind == "DaemonSet" {
			ordered = append(ordered, o)
		}
	}
	for i := len(objs) - 1; i >= 0; i-- {
		if objs[i].Kind != "DaemonSet" {
			ordered = append(ordered, objs[i])
		}
	}
	results := make([]Result, 0, len(ordered))
	var first error
	for _, o := range ordered {
		found, err := c.exists(ctx, o)
		if err == nil && !found {
			results = append(results, Result{Object: o, Action: ActionAbsent})
			continue
		}
		if err == nil {
			q := url.Values{"propagationPolicy": {"Background"}}
			if dryRun {
				q.Set("dryRun", "All")
			}
			_, err = c.do(ctx, http.MethodDelete, o.path(), q, "", nil)
			if isNotFound(err) {
				results = append(results, Result{Object: o, Action: ActionAbsent})
				continue
			}
		}
		if err != nil {
			if first == nil {
				first = err
			}
			results = append(results, Result{Object: o, Action: ActionFailed, Error: err.Error()})
			continue
		}
		results = append(results, Result{Object: o, Action: ActionDelete})
	}
	return results, first
}
