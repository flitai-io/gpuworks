package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/proto"
)

type registryAuthContextKey struct{}

// WithRegistryAuth carries the registry logins a pull may use, so the Driver
// interface and every test double stay as they are. The list is the node's
// whole set; a driver picks the entry for the registry it pulls from.
func WithRegistryAuth(ctx context.Context, auth []proto.SourceAuth) context.Context {
	if len(auth) == 0 {
		return ctx
	}
	return context.WithValue(ctx, registryAuthContextKey{}, append([]proto.SourceAuth(nil), auth...))
}

// RegistryAuthFor is the login for the registry image is pulled from.
func RegistryAuthFor(ctx context.Context, image string) (proto.SourceAuth, bool) {
	return RegistryAuthForHost(ctx, registryprobe.HostOf(image))
}

// RegistryAuthForHost is the login for a registry host; Docker Hub's spellings
// are one host. A login without a user name is a model site's token, which a
// registry never takes.
func RegistryAuthForHost(ctx context.Context, host string) (proto.SourceAuth, bool) {
	if ctx == nil {
		return proto.SourceAuth{}, false
	}
	list, _ := ctx.Value(registryAuthContextKey{}).([]proto.SourceAuth)
	host = registryprobe.NormalizeHost(host)
	for _, a := range list {
		if registryprobe.NormalizeHost(a.Host) == host && a.Username != "" {
			return a, true
		}
	}
	return proto.SourceAuth{}, false
}

// dockerHubConfigKey is where Docker's config.json keeps the Docker Hub login.
const dockerHubConfigKey = "https://index.docker.io/v1/"

// PullAuthEnv makes a CLI registry client log in for one pull. When ctx holds
// a login for the registry image comes from, it writes a private docker config
// directory holding only that login, points DOCKER_CONFIG at it in the
// returned environment, and returns a cleanup that removes the directory. The
// secret never appears in an argument list, and the user's own
// ~/.docker/config.json is neither read nor written. Without a matching login
// base is returned unchanged and cleanup is a no-op.
func PullAuthEnv(ctx context.Context, image string, base []string) (env []string, cleanup func(), err error) {
	return PullAuthEnvIn(ctx, image, base, "")
}

// PullAuthEnvIn is PullAuthEnv with the private directory created under
// parent (made, 0700, if missing) instead of the system temp directory. A CLI
// that runs in another mount namespace than the agent (the kube runtime's
// nerdctl, run on the node) only sees the paths both share, such as the
// agent's state directory.
func PullAuthEnvIn(ctx context.Context, image string, base []string, parent string) (env []string, cleanup func(), err error) {
	auth, ok := RegistryAuthFor(ctx, image)
	if !ok {
		return base, func() {}, nil
	}
	if parent != "" {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, func() {}, fmt.Errorf("prepare registry login: %w", err)
		}
	}
	dir, err := os.MkdirTemp(parent, "gpuworks-pull-auth-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("prepare registry login: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("prepare registry login: %w", err)
	}
	key := registryprobe.NormalizeHost(auth.Host)
	if key == registryprobe.DockerHub {
		key = dockerHubConfigKey
	}
	cfg := map[string]map[string]map[string]string{
		"auths": {key: {"auth": base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Secret))}},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("prepare registry login: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("prepare registry login: %w", err)
	}
	env = make([]string, 0, len(base)+1)
	for _, entry := range base {
		if k, _, _ := strings.Cut(entry, "="); k != "DOCKER_CONFIG" {
			env = append(env, entry)
		}
	}
	return append(env, "DOCKER_CONFIG="+dir), cleanup, nil
}
