package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type officialCatalogRun struct {
	t        *testing.T
	store    *store.Store
	packages *packages.Store
	router   http.Handler
	admin    string
	deps     Deps
	embedded string
}

// newOfficialCatalogRun is a started server: the embedded package stored and
// projected as the upstream catalog.
func newOfficialCatalogRun(t *testing.T) *officialCatalogRun {
	t.Helper()
	database, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	packageStore, err := packages.New(filepath.Join(t.TempDir(), "packages"))
	if err != nil {
		t.Fatal(err)
	}
	archive, digest, _, err := seed.Published()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := packageStore.PutArchive(bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	if err := seedInferenceCatalogFixture(database); err != nil {
		t.Fatal(err)
	}
	signer, admin := roleTestToken(t, database, "admin")
	deps := Deps{Store: database, CatalogPackages: packageStore, AuthSigner: signer}
	return &officialCatalogRun{t: t, store: database, packages: packageStore, router: Router(deps), admin: admin, deps: deps, embedded: digest}
}

func (r *officialCatalogRun) useChannel(channel *testChannel) {
	r.deps.Outside = channel.sources()
	r.router = Router(r.deps)
}

func (r *officialCatalogRun) do(method, path string, body []byte) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.admin)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

func (r *officialCatalogRun) upstreamDigest() string {
	r.t.Helper()
	digest, err := r.store.UpstreamInferencePackageDigest()
	if err != nil {
		r.t.Fatal(err)
	}
	return digest
}

// republishedCatalog is the embedded package with a later publication time:
// the same templates under a new digest, as the channel would carry a newer
// official catalog.
func republishedCatalog(t *testing.T) ([]byte, string) {
	t.Helper()
	return editedCatalog(t, nil)
}

// editedCatalog is republishedCatalog with edit applied to every model.yaml,
// the way a publisher for a newer server would write them.
func editedCatalog(t *testing.T, edit func(name string, data []byte) []byte) ([]byte, string) {
	t.Helper()
	archive, _, _, err := seed.Published()
	if err != nil {
		t.Fatal(err)
	}
	read, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tree := fstest.MapFS{}
	if err := fs.WalkDir(read.FS, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := fs.ReadFile(read.FS, name)
		if err != nil {
			return err
		}
		if name == "manifest.yaml" {
			manifest := read.Package.Manifest
			published, err := time.Parse(time.RFC3339, manifest.PublishedAt)
			if err != nil {
				return err
			}
			manifest.PublishedAt = published.Add(24 * time.Hour).Format(time.RFC3339)
			data, err = yaml.Marshal(manifest)
			if err != nil {
				return err
			}
		}
		if edit != nil && path.Base(name) == "model.yaml" {
			data = edit(name, data)
		}
		tree[name] = &fstest.MapFile{Data: data, Mode: 0o644}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	if err := inferencecatalog.PackPackage(&packed, tree); err != nil {
		t.Fatal(err)
	}
	digest, err := inferencecatalog.PackageDigest(tree)
	if err != nil {
		t.Fatal(err)
	}
	return packed.Bytes(), digest
}

func TestTemplateCheckForUpdatesAppliesTheChannelCatalog(t *testing.T) {
	run := newOfficialCatalogRun(t)
	archive, digest := republishedCatalog(t)
	if digest == run.embedded {
		t.Fatal("the republished package must have its own digest")
	}
	channel := newTestChannel(t)
	channel.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion)
	wantVersion := channel.index.TemplateCatalog.Version
	run.useChannel(channel)

	rec := run.do(http.MethodPost, "/api/v1/inference-templates/sync", []byte(`{"url":"http://127.0.0.1:9/evil"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", rec.Code, rec.Body.String())
	}
	var result dto.OfficialInferenceCatalogResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.PackageDigest != digest || result.Models == 0 || result.Version != wantVersion {
		t.Fatalf("result = %+v", result)
	}
	if got := run.upstreamDigest(); got != digest {
		t.Fatalf("upstream digest = %s, want %s", got, digest)
	}
	if _, err := run.packages.Load(digest); err != nil {
		t.Fatalf("the applied package is not stored: %v", err)
	}
	if got := strings.Join(channel.requested(), ","); got != "nightly/index.json,"+channel.templatePath {
		t.Fatalf("requested = %s", got)
	}
	// The update view now names the applied catalog as current.
	statusRun := &imageManifestRun{t: t, store: run.store, router: run.router, admin: run.admin}
	if c := getUpdateStatus(t, statusRun).TemplateCatalog; c.Current != wantVersion || c.Latest != c.Current || c.UpdateAvailable || !c.Compatible {
		t.Fatalf("template catalog status = %+v", c)
	}
}

func TestTemplateUploadOfOfficialCatalog(t *testing.T) {
	run := newOfficialCatalogRun(t)
	archive, digest := republishedCatalog(t)
	rec := run.do(http.MethodPost, "/api/v1/inference-templates/official", archive)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"package_digest":"`+digest+`"`) {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body.String())
	}
	if got := run.upstreamDigest(); got != digest {
		t.Fatalf("upstream digest = %s", got)
	}
	// A user catalog (YAML) is not an official package.
	yaml := run.do(http.MethodPost, "/api/v1/inference-templates/official", []byte("models: []\n"))
	if yaml.Code != http.StatusBadRequest || !strings.Contains(yaml.Body.String(), ErrorOfficialCatalogPackageRequired) {
		t.Fatalf("yaml upload = %d %s", yaml.Code, yaml.Body.String())
	}
	broken := run.do(http.MethodPost, "/api/v1/inference-templates/official", append([]byte{0x1f, 0x8b}, []byte("not gzip")...))
	if broken.Code != http.StatusBadRequest || !strings.Contains(broken.Body.String(), ErrorOfficialCatalogNeedsNewerServer) {
		t.Fatalf("broken package = %d %s", broken.Code, broken.Body.String())
	}
	if got := run.upstreamDigest(); got != digest {
		t.Fatalf("a refused upload changed the catalog: %s", got)
	}
}

// A package written for a newer server carries fields this build does not
// know. Check for updates applies everything else, reports what it left out,
// and leaves the rows it already had for those untouched.
func TestTemplateCheckForUpdatesSkipsWhatThisServerCannotRead(t *testing.T) {
	run := newOfficialCatalogRun(t)
	before, err := run.store.ListInferenceModels()
	if err != nil {
		t.Fatal(err)
	}
	// One model stated whole in a newer form, one spec of another model.
	var whole, partial, partialSpec string
	for _, model := range before {
		if len(model.Specs) == 0 || model.Source != "upstream" {
			continue
		}
		if whole == "" {
			whole = model.ModelID
		} else if len(model.Specs) > 1 && model.ModelID != whole {
			partial, partialSpec = model.ModelID, model.Specs[0].Name
			break
		}
	}
	if whole == "" || partial == "" {
		t.Fatal("the embedded catalog has no model to edit")
	}
	archive, digest := editedCatalog(t, func(name string, data []byte) []byte {
		switch name {
		case "models/" + whole + "/model.yaml":
			return append(data, []byte("future_field: true\n")...)
		case "models/" + partial + "/model.yaml":
			marker := "name: " + partialSpec + "\n"
			index := bytes.Index(data, []byte(marker))
			if index < 0 {
				t.Fatalf("spec %s not found in %s", partialSpec, name)
			}
			indent := index - bytes.LastIndexByte(data[:index], '\n') - 1
			line := strings.Repeat(" ", indent) + "future_spec_field: true\n"
			return append(append(append([]byte(nil), data[:index+len(marker)]...), line...), data[index+len(marker):]...)
		}
		return data
	})
	channel := newTestChannel(t)
	channel.publishTemplateCatalogAs(archive, digest, inferencecatalog.PackageSchemaVersion, "2026-10-20.abababab")
	run.useChannel(channel)
	statusRun := &imageManifestRun{t: t, store: run.store, router: run.router, admin: run.admin}
	if c := getUpdateStatus(t, statusRun).TemplateCatalog; !c.Compatible || !c.UpdateAvailable {
		t.Fatalf("a catalog this server reads in part is an update: %+v", c)
	}
	rec := run.do(http.MethodPost, "/api/v1/inference-templates/sync", nil)
	var result dto.OfficialInferenceCatalogResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("sync = %d %s: %v", rec.Code, rec.Body.String(), err)
	}
	if result.PackageDigest != digest {
		t.Fatalf("package digest = %s, want the published %s", result.PackageDigest, digest)
	}
	skipped := map[string]string{}
	for _, item := range result.Skipped {
		skipped[item.ModelID+"#"+item.Spec] = item.Reason
	}
	if !strings.Contains(skipped[whole+"#"], "future_field") || !strings.Contains(skipped[partial+"#"+partialSpec], "future_spec_field") {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
	if !strings.Contains(rec.Body.String(), `"model_id":"`+whole+`","reason":"`) || !strings.Contains(rec.Body.String(), `"model_id":"`+partial+`","spec":"`+partialSpec+`","reason":"`) {
		t.Fatalf("wire form of skipped: %s", rec.Body.String())
	}
	digests := func(modelID string) map[string]string {
		t.Helper()
		model, err := run.store.InferenceModelByModelID(modelID)
		if err != nil {
			t.Fatalf("%s: %v", modelID, err)
		}
		out := map[string]string{}
		for _, spec := range model.Specs {
			if spec.Source == "upstream" {
				out[spec.Name] = spec.PackageDigest
			}
		}
		return out
	}
	for name, got := range digests(whole) {
		if got != run.embedded {
			t.Fatalf("%s spec %s moved to %s; a skipped model keeps its rows", whole, name, got)
		}
	}
	for name, got := range digests(partial) {
		want := digest
		if name == partialSpec {
			want = run.embedded
		}
		if got != want {
			t.Fatalf("%s spec %s is on %s, want %s", partial, name, got, want)
		}
	}
	// The catalog in force is the published one, and the package the kept
	// rows came from is still stored for their assets.
	if got := run.upstreamDigest(); got != digest {
		t.Fatalf("upstream digest = %s, want %s", got, digest)
	}
	if c := getUpdateStatus(t, statusRun).TemplateCatalog; c.UpdateAvailable || !c.Compatible {
		t.Fatalf("status after a partial update = %+v", c)
	}
	for _, stored := range []string{digest, run.embedded} {
		if _, err := run.packages.Load(stored); err != nil {
			t.Fatalf("package %s: %v", stored, err)
		}
	}
	// The same package stays strict as a user's own import.
	imported := run.do(http.MethodPost, "/api/v1/inference-templates/import", archive)
	if imported.Code != http.StatusBadRequest || !strings.Contains(imported.Body.String(), "future_") {
		t.Fatalf("user import of the package = %d %s", imported.Code, imported.Body.String())
	}
}

// A package nobody could read is refused whole and changes nothing.
func TestTemplateCheckForUpdatesRefusesADamagedPackage(t *testing.T) {
	run := newOfficialCatalogRun(t)
	archive, digest := republishedCatalog(t)
	read, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	// Written by hand: PackPackage refuses a manifest that miscounts.
	var damaged bytes.Buffer
	gz := gzip.NewWriter(&damaged)
	tw := tar.NewWriter(gz)
	if err := fs.WalkDir(read.FS, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := fs.ReadFile(read.FS, name)
		if err != nil {
			return err
		}
		if name == "manifest.yaml" {
			manifest := read.Package.Manifest
			manifest.Specs++
			if data, err = yaml.Marshal(manifest); err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	channel := newTestChannel(t)
	channel.publishTemplateCatalogAs(damaged.Bytes(), digest, inferencecatalog.PackageSchemaVersion, "2026-10-20.abababab")
	run.useChannel(channel)
	statusRun := &imageManifestRun{t: t, store: run.store, router: run.router, admin: run.admin}
	if c := getUpdateStatus(t, statusRun).TemplateCatalog; c.Compatible || !c.UpdateAvailable {
		t.Fatalf("a damaged catalog must be incompatible before sync: %+v", c)
	}
	rec := run.do(http.MethodPost, "/api/v1/inference-templates/sync", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorOfficialCatalogNeedsNewerServer) || !strings.Contains(rec.Body.String(), "manifest says") {
		t.Fatalf("sync = %d %s", rec.Code, rec.Body.String())
	}
	if got := run.upstreamDigest(); got != run.embedded {
		t.Fatalf("a refused update changed the catalog to %s", got)
	}
}

func TestTemplateCheckForUpdatesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testChannel, []byte, string)
		status  int
		code    string
	}{
		{name: "needs a newer server", status: http.StatusConflict, code: ErrorUpdateCatalogIncompatible, prepare: func(c *testChannel, archive []byte, digest string) {
			c.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion+1)
		}},
		{name: "not published", status: http.StatusNotFound, code: ErrorUpdateNotPublished, prepare: func(*testChannel, []byte, string) {}},
		{name: "archive altered on the source", status: http.StatusBadGateway, code: ErrorUpdateDownloadFailed, prepare: func(c *testChannel, archive []byte, digest string) {
			c.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion)
			c.replace(c.templatePath, append([]byte(nil), archive[:len(archive)-1]...))
		}},
		{name: "no source answers", status: http.StatusBadGateway, code: ErrorUpdateSourcesUnavailable, prepare: func(c *testChannel, _ []byte, _ string) {
			c.fail("nightly/index.json", http.StatusServiceUnavailable)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newOfficialCatalogRun(t)
			archive, digest := republishedCatalog(t)
			channel := newTestChannel(t)
			tc.prepare(channel, archive, digest)
			run.useChannel(channel)
			rec := run.do(http.MethodPost, "/api/v1/inference-templates/sync", nil)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("sync = %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
			if got := run.upstreamDigest(); got != run.embedded {
				t.Fatalf("a refused update changed the catalog to %s", got)
			}
		})
	}
}

func seedInferenceCatalogFixture(st *store.Store) error {
	catalog, digest, err := seed.CatalogWithDigest()
	if err != nil {
		return err
	}
	return inferencecatalog.ReconcileUpstreamCatalog(st, catalog, digest)
}
