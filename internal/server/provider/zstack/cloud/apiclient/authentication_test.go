package apiclient

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func validTestAccessKey(r *http.Request) bool {
	date := r.Header.Get("Date")
	when, err := time.Parse("Mon, 02 Jan 2006 15:04:05 -07:00", date)
	if err != nil || time.Since(when).Abs() > 15*time.Minute {
		return false
	}
	mac := hmac.New(sha1.New, []byte("test-access-secret"))
	// RestServer signs the decoded URI without the servlet context or query.
	mac.Write([]byte(r.Method + "\n" + date + "\n" + strings.TrimPrefix(r.URL.Path, "/zstack")))
	expected := "ZStack test-access-key:" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(r.Header.Get("Authorization")), []byte(expected))
}

func TestAccessKeySignatureMatchesZStackDocumentation(t *testing.T) {
	// Published example: https://www.zstack.io/help/dev_manual/dev_guide/v4/3.html
	c := &APIClient{
		accessKeyID: "vvSZpmj4cnB53qUDmm6E", accessKeySecret: "8heumeFTvIeZxkTGfEYvVi9qVVPd9ffQNDALSPPb",
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/vm-instances", nil)
	c.signRequest(req, req.URL.Path, "Fri, 06 Aug 2021 17:58:34 PRC")
	if got := req.Header.Get("Authorization"); got != "ZStack vvSZpmj4cnB53qUDmm6E:hPToRHeHdV49D4u20G8OlE0yJho=" {
		t.Fatalf("signature differs from the published ZStack example: %s", got)
	}
}

func TestAccessKeySignsQueriesAndMutationsWithEndpointPrefixes(t *testing.T) {
	for _, suffix := range []string{"", "/v1", "/zstack", "/zstack/v1/"} {
		t.Run(suffix, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !validTestAccessKey(r) {
					t.Error("request did not pass ZStack AccessKey verification")
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				if strings.Contains(r.Header.Get("Authorization"), "test-access-secret") {
					t.Error("request exposed the Secret instead of a signature")
				}
				requests.Add(1)
				w.Write([]byte(`{}`))
			}))
			defer server.Close()
			c, err := New(provider.Credentials{Endpoint: server.URL + suffix,
				PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if err := c.Get(t.Context(), "/hosts?q=name=test%20host&start=0&limit=100", &out); err != nil {
				t.Fatal(err)
			}
			if err := c.PutAction(t.Context(), "/pci-device/pci-devices/gpu%2F1/actions", map[string]any{
				"updatePciDevice": map[string]any{"state": "Disabled"},
			}); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 2 {
				t.Fatalf("authenticated requests = %d", requests.Load())
			}
		})
	}
}

func TestAccessKeyRequestsPreserveLocalTimezone(t *testing.T) {
	c, err := New(provider.Credentials{Endpoint: "https://cloud.example/zstack",
		PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := c.newRequest(t.Context(), http.MethodGet, "/hosts", nil)
	if err != nil {
		t.Fatal(err)
	}
	date, err := time.Parse("Mon, 02 Jan 2006 15:04:05 -07:00", req.Header.Get("Date"))
	if err != nil {
		t.Fatal(err)
	}
	_, gotOffset := date.Zone()
	_, wantOffset := time.Now().Zone()
	if gotOffset != wantOffset || !validTestAccessKey(req) {
		t.Fatal("Date must preserve local timezone for legacy ZStack time validation")
	}
}

func TestAccessKeyCredentialsStayOnConfiguredEndpoint(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c, err := New(provider.Credentials{Endpoint: source.URL,
		PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := c.Get(t.Context(), "/hosts", &out); err == nil {
		t.Fatal("redirect was accepted")
	}
	if err := c.PutAction(t.Context(), "/pci-device/pci-devices/gpu/actions", map[string]any{}); err == nil {
		t.Fatal("redirected mutation was accepted")
	}
	if redirected.Load() != 0 {
		t.Fatal("AccessKey authorization was forwarded to another endpoint")
	}
}
