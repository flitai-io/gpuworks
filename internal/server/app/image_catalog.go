package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/community"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// EmbeddedCatalogsDue reports whether this start applies the catalogs embedded
// in the binary: only when the server build differs from the one that last
// applied them. Otherwise the database keeps whatever was imported last — an
// online update or an upload — instead of being pulled back to the embedded
// copy on every restart (ADR 0040). It also returns the build to record once
// both catalogs are applied.
func EmbeddedCatalogsDue(st *store.Store) (bool, string, error) {
	build := version.String()
	state, err := st.OfficialCatalogState()
	if err != nil {
		return false, build, err
	}
	return state.SeededBuild != build, build, nil
}

// embeddedCatalogs is one start's decision about the embedded catalogs. The
// image catalog is applied first, before anything resolves an image; the
// inference template catalog after its package is materialized.
type embeddedCatalogs struct {
	due   bool
	build string
}

func startEmbeddedCatalogs(st *store.Store, log *slog.Logger) (embeddedCatalogs, error) {
	due, build, err := EmbeddedCatalogsDue(st)
	if err != nil {
		return embeddedCatalogs{}, err
	}
	if !due {
		log.Info("official catalogs kept from their last import", "build", build)
		return embeddedCatalogs{build: build}, nil
	}
	if err := SyncImageCatalog(st, log); err != nil {
		return embeddedCatalogs{}, err
	}
	return embeddedCatalogs{due: true, build: build}, nil
}

// finish applies the embedded inference template catalog and records the
// build, when this start is applying the embedded catalogs.
func (e embeddedCatalogs) finish(st *store.Store) error {
	if !e.due {
		return nil
	}
	if err := SyncInferenceCatalog(st); err != nil {
		return err
	}
	return st.RecordEmbeddedCatalogsApplied(e.build)
}

// SyncImageCatalog brings the system image catalog to the embedded seed, logs
// what changed and records the catalog version. Callers run it when
// EmbeddedCatalogsDue says so, before anything resolves an image.
func SyncImageCatalog(st *store.Store, log *slog.Logger) error {
	result, err := imagecatalog.SyncSeed(st)
	if err != nil {
		return err
	}
	log.Info("image catalog synced", "catalog_version", result.CatalogVersion, "created", result.Created, "updated", result.Updated, "deleted", result.Deleted, "deprecated", result.Deprecated, "skipped", len(result.Skipped))
	for _, conflict := range result.Skipped {
		log.Warn("image catalog entry skipped", "kind", conflict.Kind, "name", conflict.Name, "reason", conflict.Reason)
	}
	return st.RecordImageCatalogImport(result.CatalogVersion)
}

// outsideSources builds what the API reads from outside: one community
// browser for the process, so its tag cache is shared by every request, and
// the distribution channel on its built-in sources.
func outsideSources(st *store.Store, readPrivateHosts func() []string) api.OutsideSources {
	transport := outbound.TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return planning.ControlPlaneNetwork(st) }, readPrivateHosts)
	channelTransport := outbound.TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return planning.ControlPlaneNetwork(st) }, readPrivateHosts)
	channelTransport.TLSHandshakeTimeout = 10 * time.Second
	channelTransport.ResponseHeaderTimeout = 15 * time.Second
	browser := community.NewBrowser(&http.Client{Transport: transport})
	browser.Credentials = func(host string) (string, string, error) {
		logins, err := st.SourceAuths()
		if err != nil {
			return "", "", err
		}
		for _, login := range logins {
			if strings.EqualFold(login.Host, host) && login.Username != "" {
				return login.Username, login.Secret, nil
			}
		}
		return "", "", nil
	}
	return api.OutsideSources{
		Community: browser,
		Channel:   &distribution.Client{HTTP: &http.Client{Transport: channelTransport}},
	}
}

// SyncInferenceCatalog applies the embedded package's upstream projection.
// The embedded-data package stays independent of database orchestration.
func SyncInferenceCatalog(st *store.Store) error {
	if st == nil {
		return fmt.Errorf("seed inference catalog: store is nil")
	}
	catalog, digest, err := seed.CatalogWithDigest()
	if err != nil {
		return err
	}
	return inferencecatalog.ReconcileUpstreamCatalog(st, catalog, digest)
}
