package zstackcloud_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func validCloudAccessKey(r *http.Request) bool {
	date := r.Header.Get("Date")
	when, err := time.Parse("Mon, 02 Jan 2006 15:04:05 -07:00", date)
	if err != nil || time.Since(when).Abs() > 15*time.Minute {
		return false
	}
	mac := hmac.New(sha1.New, []byte("test-access-secret"))
	mac.Write([]byte(r.Method + "\n" + date + "\n" + r.URL.Path))
	expected := "ZStack test-access-key:" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(r.Header.Get("Authorization")), []byte(expected))
}

func TestCloudAccessKeyCanReplaceStoredSessionCredentials(t *testing.T) {
	cloud := cloudInventoryFixture(t)
	env := testenv.New(t)
	master, err := os.ReadFile(filepath.Join(env.DataDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox(string(master))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(provider.Credentials{Endpoint: cloud.URL, APIKey: "expired-session"})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := box.Encrypt(legacy)
	if err != nil {
		t.Fatal(err)
	}
	p, err := env.Store().CreateProvider(&store.Provider{Name: "Cloud", Kind: "zstack", CredsCipher: cipher})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/providers/%d", p.ID)
	requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusFailedDependency)
	requestJSON(t, env, "PUT", path, `{"public_key":"test-access-key","private_key":"test-access-secret"}`, http.StatusOK)
	requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusOK)
	// Editing a name with empty credential fields retains the working key pair.
	requestJSON(t, env, "PUT", path, `{"name":"Updated Cloud","public_key":"","private_key":""}`, http.StatusOK)
	requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusOK)
	stored, err := env.Store().GetProvider(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(stored.CredsCipher), []byte("test-access-secret")) {
		t.Fatal("AccessKey Secret was stored unencrypted")
	}
	plaintext, err := box.Decrypt(stored.CredsCipher)
	if err != nil {
		t.Fatal(err)
	}
	var credentials provider.Credentials
	if err := json.Unmarshal(plaintext, &credentials); err != nil {
		t.Fatal(err)
	}
	if credentials.PublicKey != "test-access-key" || credentials.PrivateKey != "test-access-secret" {
		t.Fatal("credential update did not retain the encrypted AccessKey pair")
	}
	for _, resource := range []string{"/api/v1/providers", path + "/instances"} {
		raw := requestJSON(t, env, "GET", resource, "", http.StatusOK)
		if bytes.Contains(raw, []byte("test-access-secret")) || bytes.Contains(raw, []byte("expired-session")) {
			t.Fatal("provider response exposed credentials")
		}
	}
}
