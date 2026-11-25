package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

// verifyInput checks a channel on each source on its own, so one good source
// cannot hide a broken one.
type verifyInput struct {
	Channel string
	// Edition is the edition every index must name; empty is community.
	Edition   string
	Sources   []string
	ReleaseID string
	// Index, when set, is a signed index not published yet: each source is
	// checked for the files it lists instead of for its own index, so a
	// channel is switched only to files every source already serves.
	Index []byte
	// Full downloads every file and checks its sha256; otherwise each file is
	// checked to exist with the listed size.
	Full bool
	// Keys and HTTP replace the built-in keys and client in tests.
	Keys distribution.Keys
	HTTP *http.Client
}

func verifyCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	in := verifyInput{}
	var sources stringList
	fs.StringVar(&in.Channel, "channel", "", "channel to verify")
	fs.StringVar(&in.Edition, "edition", "", "edition the channel is published for (default: community)")
	fs.Var(&sources, "source", "source base URL (repeatable); default: the built-in sources")
	fs.StringVar(&in.ReleaseID, "release-id", "", "release id every source must offer")
	fs.BoolVar(&in.Full, "full", false, "download every file and check its sha256")
	var indexFile string
	fs.StringVar(&indexFile, "index", "", "signed index to check the sources' files against instead of their own index")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if indexFile != "" {
		raw, err := os.ReadFile(indexFile)
		if err != nil {
			return err
		}
		in.Index = raw
	}
	in.Sources = sources
	if len(in.Sources) == 0 {
		in.Sources = distribution.DefaultSources
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return verify(ctx, in, stdout)
}

func verify(ctx context.Context, in verifyInput, stdout io.Writer) error {
	var failed []string
	for _, source := range in.Sources {
		if err := verifySource(ctx, in, source); err != nil {
			fmt.Fprintf(stdout, "FAIL %s: %v\n", source, err)
			failed = append(failed, source)
			continue
		}
		fmt.Fprintf(stdout, "ok   %s\n", source)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d sources failed: %s", len(failed), len(in.Sources), strings.Join(failed, ", "))
	}
	return nil
}

func verifySource(ctx context.Context, in verifyInput, source string) error {
	client := &distribution.Client{Edition: in.Edition, Sources: []string{source}, Keys: in.Keys, HTTP: in.HTTP}
	fetched, err := sourceIndex(ctx, in, client, source)
	if err != nil {
		var sourcesErr *distribution.SourcesError
		if errors.As(err, &sourcesErr) && len(sourcesErr.Attempts) == 1 {
			return sourcesErr.Attempts[0].Err
		}
		return err
	}
	index := fetched.Index
	if in.ReleaseID != "" && index.Server.ReleaseID != in.ReleaseID {
		return fmt.Errorf("offers release %s, want %s", index.Server.ReleaseID, in.ReleaseID)
	}
	files := make([]distribution.File, 0, len(index.Server.Files)+2)
	for _, f := range index.Server.Files {
		files = append(files, f.File)
	}
	if index.ImageCatalogV4 != nil {
		files = append(files, index.ImageCatalogV4.File)
	}
	if index.ImageCatalog != nil {
		files = append(files, index.ImageCatalog.File)
	}
	if index.TemplateCatalog != nil {
		files = append(files, index.TemplateCatalog.File)
	}
	for _, file := range files {
		if in.Full {
			if _, _, err := client.ReadFile(ctx, fetched, file); err != nil {
				return err
			}
			continue
		}
		if err := headSize(ctx, client.HTTP, fetched.Source+"/"+file.Path, file.Size); err != nil {
			return err
		}
	}
	return nil
}

// sourceIndex is the index whose files are checked on source: the candidate
// index when one is given, the source's own otherwise.
func sourceIndex(ctx context.Context, in verifyInput, client *distribution.Client, source string) (*distribution.Fetched, error) {
	if in.Index == nil {
		return client.FetchIndex(ctx, in.Channel)
	}
	keys := in.Keys
	if keys == nil {
		keys = distribution.TrustedKeys()
	}
	payload, keyID, err := distribution.Open(in.Index, keys)
	if err != nil {
		return nil, fmt.Errorf("candidate index: %w", err)
	}
	index, err := distribution.ParseIndexFor(payload, in.Channel, in.Edition)
	if err != nil {
		return nil, fmt.Errorf("candidate index: %w", err)
	}
	return &distribution.Fetched{Index: index, Channel: in.Channel, Source: source, KeyID: keyID}, nil
}

// headSize checks that an object exists with the size the index lists.
func headSize(ctx context.Context, client *http.Client, url string, size int64) error {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HEAD %s: %s", url, resp.Status)
	}
	if resp.ContentLength != size {
		return fmt.Errorf("HEAD %s: %d bytes, the index lists %d", url, resp.ContentLength, size)
	}
	return nil
}
