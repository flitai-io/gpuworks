package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRegistryAuthForMatchesHostAndHubSpellings(t *testing.T) {
	ctx := WithRegistryAuth(context.Background(), []proto.SourceAuth{
		{Host: "index.docker.io", Username: "hub", Secret: "hub-secret"},
		{Host: "Quay.IO", Username: "q", Secret: "q-secret"},
		{Host: "empty.example", Username: "", Secret: "ignored"},
	})
	cases := []struct {
		image string
		user  string
		ok    bool
	}{
		{"gpuworks/runner:1", "hub", true},
		{"docker.io/gpuworks/runner:1", "hub", true},
		{"registry-1.docker.io/gpuworks/runner:1", "hub", true},
		{"ubuntu", "hub", true},
		{"quay.io/zstackai/x:1", "q", true},
		{"ghcr.io/x/y:1", "", false},
		{"empty.example/x:1", "", false},
	}
	for _, tc := range cases {
		got, ok := RegistryAuthFor(ctx, tc.image)
		if ok != tc.ok || got.Username != tc.user {
			t.Errorf("RegistryAuthFor(%q) = %q,%v want %q,%v", tc.image, got.Username, ok, tc.user, tc.ok)
		}
	}
	if _, ok := RegistryAuthFor(context.Background(), "quay.io/x:1"); ok {
		t.Fatal("a context without logins matched")
	}
	if _, ok := RegistryAuthFor(nil, "quay.io/x:1"); ok { //nolint:staticcheck // nil ctx is tolerated like PullProxy
		t.Fatal("nil context matched")
	}
	if WithRegistryAuth(context.Background(), nil) != context.Background() {
		t.Fatal("an empty login list must leave the context untouched")
	}
}

func TestPullAuthEnvWritesPrivateConfigAndCleansUp(t *testing.T) {
	ctx := WithRegistryAuth(context.Background(), []proto.SourceAuth{
		{Host: "swr.example.com", Username: "robot$gpuworks", Secret: "s3cret:with:colons"},
		{Host: "docker.io", Username: "hubuser", Secret: "hubpass"},
	})
	base := []string{"PATH=/bin", "DOCKER_CONFIG=/home/user/.docker"}

	for _, tc := range []struct{ image, key, user, secret string }{
		{"swr.example.com/gpuworks/x:1", "swr.example.com", "robot$gpuworks", "s3cret:with:colons"},
		{"gpuworks/x:1", "https://index.docker.io/v1/", "hubuser", "hubpass"},
	} {
		env, cleanup, err := PullAuthEnv(ctx, tc.image, base)
		if err != nil {
			t.Fatal(err)
		}
		dir := ""
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "DOCKER_CONFIG="); ok {
				if dir != "" {
					t.Fatalf("DOCKER_CONFIG set twice: %v", env)
				}
				dir = v
			}
		}
		if dir == "" || dir == "/home/user/.docker" {
			t.Fatalf("DOCKER_CONFIG not redirected: %v", env)
		}
		if len(env) != len(base) || env[0] != "PATH=/bin" {
			t.Fatalf("other environment entries changed: %v", env)
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("config dir %s: err=%v mode=%v", dir, err, info)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(filepath.Join(dir, "config.json")); fi.Mode().Perm() != 0o600 {
			t.Fatalf("config.json mode %v", fi.Mode().Perm())
		}
		var cfg struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
			CredsStore string `json:"credsStore"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		if len(cfg.Auths) != 1 || cfg.CredsStore != "" {
			t.Fatalf("config must hold exactly the matching login: %s", raw)
		}
		entry, ok := cfg.Auths[tc.key]
		if !ok {
			t.Fatalf("no auths[%q] in %s", tc.key, raw)
		}
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil || string(decoded) != tc.user+":"+tc.secret {
			t.Fatalf("auth = %q, %v", decoded, err)
		}
		cleanup()
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("config dir survived cleanup: %v", err)
		}
		cleanup() // idempotent
	}
}

func TestPullAuthEnvWithoutLoginLeavesEnvironmentAlone(t *testing.T) {
	ctx := WithRegistryAuth(context.Background(), []proto.SourceAuth{{Host: "other.example", Username: "u", Secret: "s"}})
	base := []string{"A=1"}
	env, cleanup, err := PullAuthEnv(ctx, "quay.io/x:1", base)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(env) != 1 || env[0] != "A=1" {
		t.Fatalf("environment changed without a matching login: %v", env)
	}
}

// PullAuthEnvIn stages the login under a caller-chosen parent (a path another
// mount namespace shares), creating the parent private, and PullAuthEnv keeps
// using the system temp directory.
func TestPullAuthEnvInUsesTheGivenParent(t *testing.T) {
	ctx := WithRegistryAuth(context.Background(), []proto.SourceAuth{{Host: "quay.io", Username: "u", Secret: "s"}})
	parent := filepath.Join(t.TempDir(), "state", "pull-auth")
	env, cleanup, err := PullAuthEnvIn(ctx, "quay.io/x:1", nil, parent)
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.TrimPrefix(env[len(env)-1], "DOCKER_CONFIG=")
	if filepath.Dir(dir) != parent {
		t.Fatalf("login dir %s is not under %s", dir, parent)
	}
	if info, err := os.Stat(parent); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("parent: %v %v", info, err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("login dir: %v %v", info, err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("login dir survived cleanup: %v", err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("cleanup removed the shared parent: %v", err)
	}
	// A parent that cannot be made is an error, not a fallback to /tmp.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if env, _, err := PullAuthEnvIn(ctx, "quay.io/x:1", nil, filepath.Join(blocker, "sub")); err == nil || env != nil {
		t.Fatalf("env=%v err=%v", env, err)
	}
}

func TestPullAuthEnvFailsWhenTempDirUnavailable(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing", "dir"))
	ctx := WithRegistryAuth(context.Background(), []proto.SourceAuth{{Host: "quay.io", Username: "u", Secret: "never-in-errors"}})
	env, cleanup, err := PullAuthEnv(ctx, "quay.io/x:1", nil)
	if err == nil || env != nil {
		t.Fatalf("temp dir failure must be an error: env=%v err=%v", env, err)
	}
	if strings.Contains(err.Error(), "never-in-errors") {
		t.Fatalf("error leaks the secret: %v", err)
	}
	cleanup()
}
