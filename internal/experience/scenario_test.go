package experience

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReply(t *testing.T) {
	got := reply("Qwen/qwen3-8b-chat-ab12", json.RawMessage(`[{"role":"user","content":"你是谁"}]`))
	if !strings.Contains(got, "qwen3-8b-chat 模型") || !strings.Contains(got, "「你是谁」") || !strings.Contains(got, "模拟") {
		t.Fatalf("reply %q", got)
	}
	if got := reply("", json.RawMessage(`[]`)); !strings.Contains(got, "GPUWorks 模型") {
		t.Fatalf("reply without model or question %q", got)
	}
}

func TestLastUserMessage(t *testing.T) {
	cases := map[string]string{
		`[{"role":"user","content":" first "},{"role":"assistant","content":"x"},{"role":"user","content":"second"}]`: "second",
		`[{"role":"user","content":[{"type":"image_url"},{"type":"text","text":" look "}]}]`:                          "look",
		`[{"role":"system","content":"s"}]`: "",
		`not json`:                          "",
		`[{"role":"user","content":123}]`:   "",
	}
	for raw, want := range cases {
		if got := lastUserMessage(json.RawMessage(raw)); got != want {
			t.Errorf("lastUserMessage(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("一二三四", 2); got != "一二…" {
		t.Fatalf("%q", got)
	}
	if got := truncateRunes("一二", 2); got != "一二" {
		t.Fatalf("%q", got)
	}
}

func TestScenarioIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range scenario {
		if seen[n.Hostname] {
			t.Fatalf("duplicate node %s", n.Hostname)
		}
		seen[n.Hostname] = true
	}
	for _, st := range starters {
		if st.Node != "" && !seen[st.Node] {
			t.Errorf("starter %s pins unknown node %s", st.Name, st.Node)
		}
		if _, err := catalogImage(st.Repo, st.Engine, st.Vendor); err != nil {
			t.Errorf("starter %s: %v", st.Name, err)
		}
	}
}

// The model list needs at least two preloaded models that can be selected
// together and removed without deleting any of the starter deployments.
func TestScenarioHasUnusedModels(t *testing.T) {
	shapes, err := catalogShapes()
	if err != nil {
		t.Fatal(err)
	}
	unused := map[string]bool{}
	for _, n := range scenario {
		for _, repo := range n.Models {
			if _, ok := shapes[strings.ToLower(repo)]; !ok {
				t.Fatalf("no catalog shape for %s", repo)
			}
			unused[repo] = true
		}
	}
	for _, st := range starters {
		delete(unused, st.Repo)
	}
	if len(unused) < 2 {
		t.Fatalf("only %d unused model(s): %v", len(unused), unused)
	}
}

func TestCatalogImage(t *testing.T) {
	image, err := catalogImage("Qwen/Qwen3-32B", "sglang", "ascend")
	if err != nil || !strings.Contains(image, "910b") {
		t.Fatalf("ascend image %q, %v", image, err)
	}
	if _, err := catalogImage("Qwen/Qwen3-32B", "vllm", "nosuchvendor"); err == nil {
		t.Fatal("an image for an unknown vendor")
	}
}

// fakeConsole answers login with a token per password version and
// refuses calls made with an older token.
type fakeConsole struct {
	password atomic.Value
	logins   atomic.Int32
	secret   atomic.Value
}

func (f *fakeConsole) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.secret.Store(r.Header.Get(internalHeader))
	if r.URL.Path == "/api/v1/auth/login" {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["password"] != f.password.Load().(string) {
			http.Error(w, "bad password", http.StatusUnauthorized)
			return
		}
		f.logins.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "t-" + body["password"]})
		return
	}
	if r.Header.Get("Authorization") != "Bearer t-"+f.password.Load().(string) {
		http.Error(w, "expired", http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"ok": "yes"})
}

func TestClientLogsInAgainAfterPasswordChange(t *testing.T) {
	f := &fakeConsole{}
	f.password.Store("one")
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	c, err := login(ctx, srv.URL, "admin", "one", "s3")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := c.get(ctx, "/api/v1/x", &out); err != nil || out["ok"] != "yes" {
		t.Fatalf("get: %v %v", out, err)
	}
	if f.secret.Load() != "s3" {
		t.Fatalf("secret header %v", f.secret.Load())
	}
	f.password.Store("two")
	if err := c.get(ctx, "/api/v1/x", &out); err == nil {
		t.Fatal("a call succeeded with the old password")
	}
	c.setPassword("two")
	if err := c.post(ctx, "/api/v1/x", map[string]int{"a": 1}, &out); err != nil {
		t.Fatalf("post after the password change: %v", err)
	}
	if n := f.logins.Load(); n != 2 {
		t.Fatalf("%d logins", n)
	}
	if _, err := login(ctx, srv.URL, "admin", "wrong", ""); err == nil {
		t.Fatal("login with a wrong password")
	}
}

func TestRetry(t *testing.T) {
	calls := 0
	err := retry(context.Background(), 3, func() error {
		calls++
		if calls < 2 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := retry(ctx, 5, func() error { return errors.New("never") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retry: %v", err)
	}
}

func TestPickDeploymentFollowsShares(t *testing.T) {
	running := []deployment{{Name: "qwen3-32b"}, {Name: "qwen3-32b-ascend"}}
	counts := map[string]int{}
	for i := 0; i < 4000; i++ {
		counts[pickDeployment(running).Name]++
	}
	if counts["qwen3-32b"] < 2*counts["qwen3-32b-ascend"] {
		t.Fatalf("shares not followed: %v", counts)
	}
}
