package api

import (
	"bytes"
	"context"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// A schema number alone cannot establish compatibility: a package can add
// fields or validation rules without changing its archive format. Keep the
// parser's verdict for the same probe the index cache serves, so polling the
// update view does not download and parse the catalog again. A refreshed
// index also retries a failed download.
type templateUpdateCheck struct {
	mu         sync.Mutex
	fetched    *distribution.Fetched
	compatible bool
	err        error
	at         time.Time
}

func (c *templateUpdateCheck) check(ctx context.Context, client *distribution.Client, fetched *distribution.Fetched, allowFetch bool) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetched == fetched && (c.err == nil || !allowFetch || time.Since(c.at) < distribution.FailedCheckTTL) {
		return c.compatible, c.err
	}
	if !allowFetch {
		// Automatic checks may have been disabled after an index prefetch.
		// Until a manual check validates its package, do not claim compatibility
		// or contact the sources from a page load.
		return false, nil
	}
	raw, _, err := client.ReadFile(ctx, fetched, fetched.Index.TemplateCatalog.File)
	compatible := false
	if err == nil {
		// Applying the package leaves out what this build cannot read, so
		// only a package it cannot read at all is incompatible.
		_, _, parseErr := inferencecatalog.ReadPackageArchiveTolerant(bytes.NewReader(raw))
		compatible = parseErr == nil
	}
	if ctx.Err() != nil {
		// A disconnected caller says nothing about the published catalog.
		return false, ctx.Err()
	}
	c.fetched, c.compatible, c.err = fetched, compatible, err
	c.at = time.Now().UTC()
	return compatible, err
}
