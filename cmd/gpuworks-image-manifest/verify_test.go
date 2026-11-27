package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ociIndex    = "application/vnd.oci.image.index.v1+json"
	ociManifest = "application/vnd.oci.image.manifest.v1+json"
	dockerList  = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// The hosts the shipped rewrite rules mirror gpuworks-runner to.
var verifyHosts = []string{"docker.io", "quay.io", "swr.cn-southwest-2.myhuaweicloud.com"}

const verifyMatrix = `defaults:
  platforms: [linux/amd64, linux/arm64]
items:
  - id: cuda13.0-vllm0.30.0
    revision: r2
  - id: dtk26.04-vllm0.18.1
    revision: r2
    platforms: [linux/amd64]
`

type storedManifest struct {
	mediaType string
	body      []byte
}

// fakeRegistry answers the distribution API the way the public registries
// do: an anonymous Bearer challenge, manifests negotiated by Accept, HEAD
// that carries Docker-Content-Digest, JSON errors on 404.
type fakeRegistry struct {
	t      *testing.T
	server *httptest.Server
	host   string
	// rewriteIndex stores an index as a Docker manifest list, as SWR does,
	// which changes the index digest but not the platform manifests.
	rewriteIndex bool
	// blobRedirect answers blob HEADs with a redirect to storage, as Docker Hub
	// and quay do.
	blobRedirect bool
	// basicOnly challenges for credentials instead of an anonymous token.
	basicOnly bool

	mu           sync.Mutex
	tags         map[string]string
	manifests    map[string]storedManifest
	blobs        map[string]bool
	manifestGETs int
	failNext     int
}

func newFakeRegistry(t *testing.T, host string) *fakeRegistry {
	r := &fakeRegistry{t: t, host: host, tags: map[string]string{}, manifests: map[string]storedManifest{}, blobs: map[string]bool{}}
	r.server = httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	r.server.Config.ErrorLog = log.New(io.Discard, "", 0) // clients still dialing at teardown
	r.server.StartTLS()
	t.Cleanup(r.server.Close)
	return r
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext > 0 {
		r.failNext--
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if req.URL.Path == "/token" {
		if req.URL.Query().Get("service") != r.host || !strings.HasPrefix(req.URL.Query().Get("scope"), "repository:") {
			http.Error(w, `{"errors":[{"code":"DENIED"}]}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon " + req.URL.Query().Get("scope")})
		return
	}
	rest := strings.TrimPrefix(req.URL.Path, "/v2/")
	kind := "/manifests/"
	at := strings.LastIndex(rest, kind)
	if at < 0 {
		kind = "/blobs/"
		at = strings.LastIndex(rest, kind)
	}
	if at < 0 {
		http.NotFound(w, req)
		return
	}
	repo, reference := rest[:at], rest[at+len(kind):]
	scope := "repository:" + repo + ":pull"
	if req.Header.Get("Authorization") != "Bearer anon "+scope {
		if r.basicOnly {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		} else {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.server.URL+`/token",service="`+r.host+`",scope="`+scope+`"`)
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if kind == "/blobs/" {
		switch {
		case !r.blobs[repo+"@"+reference]:
			w.WriteHeader(http.StatusNotFound)
		case r.blobRedirect:
			w.Header().Set("Location", "https://storage.example/"+reference)
			w.WriteHeader(http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	digest := reference
	if !strings.HasPrefix(reference, "sha256:") {
		digest = r.tags[repo+":"+reference]
	}
	stored, ok := r.manifests[repo+"@"+digest]
	if !ok || !strings.Contains(req.Header.Get("Accept"), stored.mediaType) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if req.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`))
		}
		return
	}
	w.Header().Set("Content-Type", stored.mediaType)
	w.Header().Set("Docker-Content-Digest", digest)
	if req.Method == http.MethodGet {
		r.manifestGETs++
		_, _ = w.Write(stored.body)
	}
}

// testImage is one built image: its index and every manifest and blob under it.
type testImage struct {
	index     []byte
	children  map[string][]byte
	platforms map[string]string
	blobs     []string
}

func buildImage(t *testing.T, seed string, platforms ...string) testImage {
	t.Helper()
	img := testImage{children: map[string][]byte{}, platforms: map[string]string{}}
	base := sha256Digest([]byte(seed + " base layer"))
	type entry struct {
		MediaType string            `json:"mediaType"`
		Digest    string            `json:"digest"`
		Size      int               `json:"size"`
		Platform  map[string]string `json:"platform,omitempty"`
	}
	var entries []entry
	for _, platform := range platforms {
		config := sha256Digest([]byte(seed + platform + " config"))
		layer := sha256Digest([]byte(seed + platform + " layer"))
		img.blobs = append(img.blobs, config, layer)
		body, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": ociManifest,
			"config": entry{MediaType: "application/vnd.oci.image.config.v1+json", Digest: config, Size: 10},
			"layers": []entry{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: base, Size: 10}, {MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: layer, Size: 10}},
		})
		digest := sha256Digest(body)
		img.children[digest] = body
		img.platforms[platform] = digest
		os, arch, _ := strings.Cut(platform, "/")
		entries = append(entries, entry{MediaType: ociManifest, Digest: digest, Size: len(body), Platform: map[string]string{"os": os, "architecture": arch}})
	}
	img.blobs = append(img.blobs, base)
	// A build attestation rides along in the index; its blobs are never pushed.
	attestation, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ociManifest,
		"config": entry{MediaType: "application/vnd.oci.image.config.v1+json", Digest: sha256Digest([]byte(seed + " attestation")), Size: 1}})
	img.children[sha256Digest(attestation)] = attestation
	entries = append(entries, entry{MediaType: ociManifest, Digest: sha256Digest(attestation), Size: len(attestation), Platform: map[string]string{"os": "unknown", "architecture": "unknown"}})
	img.index, _ = json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ociIndex, "manifests": entries})
	return img
}

func (r *fakeRegistry) push(repo, tag string, img testImage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for digest, body := range img.children {
		r.manifests[repo+"@"+digest] = storedManifest{mediaType: ociManifest, body: body}
	}
	for _, blob := range img.blobs {
		r.blobs[repo+"@"+blob] = true
	}
	index := storedManifest{mediaType: ociIndex, body: img.index}
	if r.rewriteIndex {
		var doc map[string]any
		_ = json.Unmarshal(img.index, &doc)
		doc["mediaType"] = dockerList
		index.body, _ = json.Marshal(doc)
		index.mediaType = dockerList
	}
	digest := sha256Digest(index.body)
	r.manifests[repo+"@"+digest] = index
	r.tags[repo+":"+tag] = digest
}

type verifyFixture struct {
	manifest   []byte
	registries map[string]*fakeRegistry
	images     map[string]testImage
	verifier   *verifier
}

// newVerifyFixture publishes the generated catalog for verifyMatrix to all
// four registries, whole.
func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	data, err := generate([]byte(verifyMatrix), options{Registry: DefaultRegistry, Date: "2026-10-09"})
	if err != nil {
		t.Fatal(err)
	}
	f := &verifyFixture{manifest: data, registries: map[string]*fakeRegistry{}, images: map[string]testImage{
		"cuda13.0-vllm0.30.0-r2": buildImage(t, "cuda", "linux/amd64", "linux/arm64"),
		"dtk26.04-vllm0.18.1-r2": buildImage(t, "dtk", "linux/amd64"),
	}}
	var client *http.Client
	for _, host := range verifyHosts {
		reg := newFakeRegistry(t, host)
		reg.rewriteIndex = strings.HasPrefix(host, "swr.")
		reg.blobRedirect = host == "docker.io" || host == "quay.io"
		for tag, img := range f.images {
			reg.push("gpuworks/gpuworks-runner", tag, img)
		}
		f.registries[host] = reg
		client = reg.server.Client() // every httptest TLS server trusts the same certificate
	}
	f.verifier = newVerifier(client, func(host string) string {
		reg, ok := f.registries[host]
		if !ok {
			t.Errorf("request to unexpected registry %s", host)
			return "https://unexpected.invalid"
		}
		return reg.server.URL
	})
	f.verifier.backoff = time.Millisecond
	return f
}

func (f *verifyFixture) run(t *testing.T) verifyReport {
	t.Helper()
	report, err := verifyManifest(context.Background(), f.manifest, f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestVerifyAcceptsAWholeCatalog(t *testing.T) {
	f := newVerifyFixture(t)
	report := f.run(t)
	if len(report.Problems) != 0 {
		t.Fatalf("problems = %q", report.Problems)
	}
	// Two versions, each named on three registries; the attestation is not a
	// platform and its unpushed blobs are not asked for.
	if report.Refs != 6 || report.Platforms != 9 || report.Blobs != 3*(5+3) {
		t.Fatalf("report = %+v", report)
	}
	if got := f.registries["docker.io"].manifestGETs; got != 0 {
		t.Fatalf("Docker Hub served %d manifest GETs; its bodies are cached from the mirrors, it is only HEADed", got)
	}
	if f.registries["swr.cn-southwest-2.myhuaweicloud.com"].manifestGETs == 0 {
		t.Fatal("SWR rewrote the index, so its own index must be read")
	}
}

func TestVerifyReportsWhatAPullWouldTrip(t *testing.T) {
	const repo = "gpuworks/gpuworks-runner"
	for name, tc := range map[string]struct {
		break_ func(f *verifyFixture)
		want   []string
	}{
		"a mirror lost the platform manifests its tag lists": {
			break_: func(f *verifyFixture) {
				reg := f.registries["swr.cn-southwest-2.myhuaweicloud.com"]
				delete(reg.manifests, repo+"@"+f.images["cuda13.0-vllm0.30.0-r2"].platforms["linux/arm64"])
			},
			want: []string{"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2: linux/arm64 manifest " + "%arm64%" + ": not found"},
		},
		"the catalog names a revision nobody published": {
			break_: func(f *verifyFixture) {
				for _, reg := range f.registries {
					delete(reg.tags, repo+":dtk26.04-vllm0.18.1-r2")
				}
			},
			want: []string{
				"gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2: tag not found",
				"quay.io/gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2: tag not found",
				"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2: tag not found",
			},
		},
		"a mirror is missing a layer": {
			break_: func(f *verifyFixture) {
				delete(f.registries["quay.io"].blobs, repo+"@"+f.images["dtk26.04-vllm0.18.1-r2"].blobs[1])
			},
			want: []string{"quay.io/gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2: linux/amd64 blob " + "%dtklayer%" + ": not found"},
		},
		"a mirror serves another build under the tag": {
			break_: func(f *verifyFixture) {
				f.registries["swr.cn-southwest-2.myhuaweicloud.com"].push(repo, "dtk26.04-vllm0.18.1-r2", buildImage(t, "rebuilt dtk", "linux/amd64"))
			},
			want: []string{"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2: serves different content than gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2: linux/amd64 " + "%dtkamd64%" + " vs " + "%rebuilt%"},
		},
		"a mirror wants credentials": {
			break_: func(f *verifyFixture) { f.registries["quay.io"].basicOnly = true },
			want: []string{
				`quay.io/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2: registry refuses anonymous pulls (challenge "Basic realm=\"registry\"")`,
				`quay.io/gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2: registry refuses anonymous pulls (challenge "Basic realm=\"registry\"")`,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newVerifyFixture(t)
			tc.break_(f)
			report := f.run(t)
			replacer := strings.NewReplacer(
				"%arm64%", f.images["cuda13.0-vllm0.30.0-r2"].platforms["linux/arm64"],
				"%dtklayer%", f.images["dtk26.04-vllm0.18.1-r2"].blobs[1],
				"%dtkamd64%", f.images["dtk26.04-vllm0.18.1-r2"].platforms["linux/amd64"],
				"%rebuilt%", buildImage(t, "rebuilt dtk", "linux/amd64").platforms["linux/amd64"],
			)
			var want []string
			for _, line := range tc.want {
				want = append(want, replacer.Replace(line))
			}
			if strings.Join(report.Problems, "\n") != strings.Join(want, "\n") {
				t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(report.Problems, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

func TestVerifyRetriesATransientRegistryError(t *testing.T) {
	f := newVerifyFixture(t)
	f.registries["quay.io"].failNext = 2
	if report := f.run(t); len(report.Problems) != 0 {
		t.Fatalf("two 503s within the retry budget must not fail the check: %q", report.Problems)
	}
	f = newVerifyFixture(t)
	f.registries["swr.cn-southwest-2.myhuaweicloud.com"].failNext = 1000
	report := f.run(t)
	if len(report.Problems) != 2 || !strings.Contains(report.Problems[0], "HTTP 503") {
		t.Fatalf("a registry that keeps failing is reported: %q", report.Problems)
	}
}

func TestVerifyRejectsAnInvalidManifest(t *testing.T) {
	if _, err := verifyManifest(context.Background(), []byte(`{"schema_version":1}`), newVerifier(nil, nil)); err == nil {
		t.Fatal("an unparseable manifest is an error, not an empty report")
	}
}

func TestVerifyRejectsChangedPinnedManifest(t *testing.T) {
	f := newVerifyFixture(t)
	const tag = "cuda13.0-vllm0.30.0-r2"
	for _, sources := range []string{"", "    registry_sources: []\n"} {
		matrix := "items:\n  - id: cuda13.0-vllm0.30.0\n    revision: r2\n    builder: prebuilt\n    backend: cuda\n" + sources + "    artifact: quay.io/gpuworks/gpuworks-runner@sha256:" + strings.Repeat("0", 64) + "\n    platforms: [linux/amd64,linux/arm64]\n"
		body, err := generate([]byte(matrix), options{Date: "2026-11-26"})
		if err != nil {
			t.Fatal(err)
		}
		pins, err := publicationPins([]byte(matrix), body)
		if err != nil || pins[tag] != "quay.io/gpuworks/gpuworks-runner:"+tag {
			t.Fatalf("pins=%v err=%v", pins, err)
		}
		changed := strings.Replace(matrix, strings.Repeat("0", 64), strings.Repeat("1", 64), 1)
		if _, err := publicationPins([]byte(changed), body); err == nil {
			t.Fatal("publication accepted a stale digest")
		}
		f.manifest = body
		f.verifier.pinnedSources = pins
		if _, err := verifyPublicationPins(context.Background(), body, f.verifier); err == nil || !strings.Contains(err.Error(), "matrix pins") {
			t.Fatalf("publication accepted invalid digest: %v", err)
		}
		good := strings.Replace(matrix, "sha256:"+strings.Repeat("0", 64), f.registries["quay.io"].tags["gpuworks/gpuworks-runner:"+tag], 1)
		goodBody, err := generate([]byte(good), options{Date: "2026-11-26"})
		if err != nil {
			t.Fatal(err)
		}
		if n, err := verifyPublicationPins(context.Background(), goodBody, f.verifier); err != nil || n != 1 {
			t.Fatalf("valid publication n=%d err=%v", n, err)
		}
		report := f.run(t)
		if len(report.Problems) != 1 || !strings.Contains(report.Problems[0], "quay.io/gpuworks/gpuworks-runner:"+tag+": registry digest") {
			t.Fatalf("wrong digest check: %+v", report)
		}
	}
}
