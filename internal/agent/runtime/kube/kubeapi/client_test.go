package kubeapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientDecodesStatusErrorsAndSendsTheToken(t *testing.T) {
	var gotAuth, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT = string(b), r.Header.Get("Content-Type")
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(409)
			_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","message":"pods \"x\" already exists","reason":"AlreadyExists","code":409}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
		case r.Method == http.MethodPatch:
			_, _ = io.WriteString(w, `{"metadata":{"name":"ds"}}`)
		default:
			w.WriteHeader(502)
			_, _ = io.WriteString(w, "bad gateway from a proxy")
		}
	}))
	defer srv.Close()
	c := New(srv.URL, func() (string, error) { return "tok", nil }, srv.Client())
	err := c.Create(context.Background(), PodsPath("ns"), Pod{Metadata: ObjectMeta{Name: "x"}}, nil)
	if !IsAlreadyExists(err) || gotAuth != "Bearer tok" || gotCT != "application/json" || !strings.Contains(gotBody, `"name":"x"`) {
		t.Fatalf("create: err=%v auth=%q ct=%q body=%s", err, gotAuth, gotCT, gotBody)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != `pods "x" already exists` {
		t.Fatalf("APIError = %+v", apiErr)
	}
	grace := int64(0)
	if err := c.Delete(context.Background(), PodPath("ns", "x"), &grace); err != nil {
		t.Fatalf("delete of a missing pod: %v", err)
	}
	var opts map[string]any
	if json.Unmarshal([]byte(gotBody), &opts) != nil || opts["gracePeriodSeconds"] != float64(0) || opts["kind"] != "DeleteOptions" {
		t.Fatalf("delete options = %s", gotBody)
	}
	if err := c.MergePatch(context.Background(), DaemonSetPath("ns", "ds"), map[string]any{"a": 1}, nil); err != nil || gotCT != "application/merge-patch+json" {
		t.Fatalf("patch: %v ct=%q", err, gotCT)
	}
	err = c.Get(context.Background(), NodePath("n"), &Node{})
	if !errors.As(err, &apiErr) || apiErr.Code != 502 || !strings.Contains(apiErr.Message, "bad gateway") || IsNotFound(err) {
		t.Fatalf("non-Status error = %v", err)
	}
	bad := New(srv.URL, func() (string, error) { return "", errors.New("token unreadable") }, srv.Client())
	if err := bad.Get(context.Background(), NodePath("n"), &Node{}); err == nil || !strings.Contains(err.Error(), "token unreadable") {
		t.Fatalf("token error = %v", err)
	}
}

func TestListQueryAndPaths(t *testing.T) {
	q := ListQuery("a=b", map[string]string{"spec.nodeName": "n1", "metadata.name": "x"})
	if q != "?fieldSelector=metadata.name%3Dx%2Cspec.nodeName%3Dn1&labelSelector=a%3Db&resourceVersion=0" {
		t.Fatalf("ListQuery = %q", q)
	}
	if PodPath("ns", "p") != "/api/v1/namespaces/ns/pods/p" || DaemonSetPath("ns", "d") != "/apis/apps/v1/namespaces/ns/daemonsets/d" {
		t.Fatal("paths")
	}
}

func TestInClusterNeedsTheEnvironment(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := InCluster(); err == nil {
		t.Fatal("InCluster outside a pod succeeded")
	}
}
