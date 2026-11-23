package app

import (
	"context"
	"log/slog"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// applyStartupState brings the database and the catalog package files to
// what this binary expects, before anything reads them. It runs once per
// start of a server that owns its store.
func applyStartupState(cfg Config, st *store.Store, catalogPackages *packages.Store, log *slog.Logger) error {
	embedded, err := startEmbeddedCatalogs(st, log)
	if err != nil {
		return err
	}
	// Bring stored deployments to the current launch-configuration shape before
	// anything reads one. The controller launches instances straight from these
	// columns, so a row still in the old shape would be started from a
	// configuration that is missing values nobody merges in any more.
	if err := normalizeLaunchConfigs(st); err != nil {
		return err
	}
	// Materialize the exact package embedded in this binary before updating the
	// database projection. A later server version then keeps this directory as
	// long as an older deployment or instance snapshot still references it.
	embeddedCatalog, embeddedDigest, err := seed.Package()
	if err != nil {
		return err
	}
	if _, err := catalogPackages.PutLoaded(embeddedCatalog, embeddedDigest); err != nil {
		return err
	}
	if err := embedded.finish(st); err != nil {
		return err
	}
	if recovered, err := st.RecoverServerBenchmarkRuns(); err != nil {
		return err
	} else if recovered > 0 {
		log.Warn("recovered orphaned server benchmarks", "count", recovered)
	}
	// Express the configured model endpoint as site rules, the one place a
	// node's download addresses come from, so node downloads stay hermetic
	// without a second, implicit endpoint setting.
	if endpoint := strings.TrimSpace(cfg.ModelEndpoint); endpoint != "" {
		return ensureModelEndpointRules(st, endpoint, cfg.Experience != nil)
	}
	return nil
}

// pruneCatalogPackages removes the package directories no deployment or
// instance snapshot references any more.
func pruneCatalogPackages(st *store.Store, catalogPackages *packages.Store, log *slog.Logger) {
	if referenced, referenceErr := st.CatalogPackageReferencesForGC(context.Background()); referenceErr != nil {
		log.Warn("list referenced inference catalog packages during startup", "error", referenceErr)
	} else if removed, pruneErr := catalogPackages.Prune(referenced); pruneErr != nil {
		log.Warn("prune inference catalog packages during startup", "error", pruneErr)
	} else if len(removed) > 0 {
		log.Info("pruned unreferenced inference catalog packages during startup", "count", len(removed))
	}
}
