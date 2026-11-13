package digitalocean

import (
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestAuthorizePublicKeyRegistersTheKeyAndKeepsTheOthers(t *testing.T) {
	d, f := newFake(t)
	userLine, _ := newTestKey(t, "MatheMatrix@foxmail.com")
	f.addKey(t, "MatheMatrix@foxmail.com", userLine)
	userKey := f.keys[0]

	platform, platformFP := newTestKey(t, "gpuworks")
	detail, err := d.AuthorizePublicKey(t.Context(), testCreds, "", platform+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "afterwards") || !strings.Contains(detail, "SSH key or password") {
		t.Errorf("detail does not say that only later droplets get the key: %q", detail)
	}
	posts := f.calls(http.MethodPost, "/v2/account/keys")
	if len(posts) != 1 || posts[0].Body["name"] != "gpuworks-platform" || posts[0].Body["public_key"] != platform || len(posts[0].Body) != 2 {
		t.Fatalf("POST /v2/account/keys = %+v", posts)
	}
	if len(f.keys) != 2 || f.keys[1]["fingerprint"] != platformFP {
		t.Errorf("keys = %v", f.keys)
	}
	if f.keys[0]["id"] != userKey["id"] || f.keys[0]["name"] != "MatheMatrix@foxmail.com" || f.keys[0]["public_key"] != userLine {
		t.Errorf("the user's key changed: %v", f.keys[0])
	}
	for _, r := range f.requests {
		if r.Method != http.MethodGet && !(r.Method == http.MethodPost && r.Path == "/v2/account/keys") {
			t.Errorf("unexpected write %s %s", r.Method, r.Path)
		}
	}
}

func TestAuthorizePublicKeyReusesAKeyAlreadyInTheAccount(t *testing.T) {
	d, f := createFake(t)
	line, fingerprint := newTestKey(t, "uploaded by hand")
	f.addKey(t, "my-laptop", line)
	// The same key, as the server holds it: another comment.
	detail, err := d.AuthorizePublicKey(t.Context(), testCreds, "401", strings.Fields(line)[0]+" "+strings.Fields(line)[1]+" gpuworks-platform")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "already") || !strings.Contains(detail, "my-laptop") {
		t.Errorf("detail = %q", detail)
	}
	if n := len(f.calls(http.MethodPost, "/v2/account/keys")); n != 0 || len(f.keys) != 1 {
		t.Errorf("the key was registered again: %d posts, %d keys", n, len(f.keys))
	}
	// A droplet created next gets that key, whatever its name.
	if _, err := d.CreateInstance(t.Context(), testCreds, mustRegion("nyc2", "gpu-h100x1-80gb")); err != nil {
		t.Fatal(err)
	}
	keys, _ := lastCreate(t, f)["ssh_keys"].([]any)
	if len(keys) != 1 || keys[0] != fingerprint {
		t.Errorf("ssh_keys = %v, want only %s", keys, fingerprint)
	}
}

func TestAuthorizePublicKeyRefusesAMalformedKey(t *testing.T) {
	d, f := newFake(t)
	for _, key := range []string{"", "   ", "ssh-ed25519"} {
		if _, err := d.AuthorizePublicKey(t.Context(), testCreds, "", key); err == nil {
			t.Errorf("accepted %q", key)
		}
	}
	if len(f.requests) != 0 {
		t.Errorf("requests sent: %v", f.requests)
	}
}

func TestAuthorizePublicKeyFollowsTheKeyListPages(t *testing.T) {
	d, f := newFake(t)
	line, _ := newTestKey(t, "")
	for i := 0; i < 205; i++ {
		other, _ := newTestKey(t, "")
		f.addKey(t, "k", other)
	}
	f.addKey(t, "on-page-2", line)
	if _, err := d.AuthorizePublicKey(t.Context(), testCreds, "", line); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls(http.MethodPost, "/v2/account/keys")); n != 0 {
		t.Errorf("a key on the second page was registered again")
	}
}

func TestAuthorizePublicKeyErrors(t *testing.T) {
	line, _ := newTestKey(t, "")
	d, f := newFake(t)
	if _, err := d.AuthorizePublicKey(t.Context(), badCreds, "", line); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("bad token: %v", err)
	}
	f.pageFailures = map[string]int{"1": http.StatusForbidden}
	if _, err := d.AuthorizePublicKey(t.Context(), testCreds, "", line); err == nil {
		t.Error("a failed key listing was ignored")
	}
}

// With no key registered by this server, a key named gpuworks-platform in
// the account is the platform key a created droplet gets.
func TestCreateFindsThePlatformKeyByName(t *testing.T) {
	d, f := createFake(t)
	line, fingerprint := newTestKey(t, "")
	other, _ := newTestKey(t, "")
	f.addKey(t, "someone-else", other)
	f.addKey(t, "gpuworks-platform", line)
	if _, err := d.CreateInstance(t.Context(), testCreds, mustRegion("nyc2", "gpu-h100x1-80gb")); err != nil {
		t.Fatal(err)
	}
	keys, _ := lastCreate(t, f)["ssh_keys"].([]any)
	if len(keys) != 1 || keys[0] != fingerprint {
		t.Errorf("ssh_keys = %v, want only %s", keys, fingerprint)
	}
}

func mustRegion(region, size string) provider.CreateInstanceRequest {
	return provider.CreateInstanceRequest{Region: region, Extra: map[string]string{"size": size}}
}
