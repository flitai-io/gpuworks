package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

func writeSeedManifest(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("seed-manifest", flag.ContinueOnError)
	catalog := flags.String("catalog", "", "seed source directory")
	date := flags.String("published-at", "", "catalog commit timestamp (RFC3339); omit for an unpublished build")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *catalog == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: gpuworks-pack-catalog seed-manifest --catalog DIR [--published-at TIMESTAMP]")
	}
	var publishedAt time.Time
	if *date != "" {
		parsed, err := time.Parse(time.RFC3339, *date)
		if err != nil {
			return fmt.Errorf("seed publication time: %w", err)
		}
		publishedAt = parsed
	}
	data, err := inferencecatalog.BuildSeedManifest(os.DirFS(*catalog), publishedAt)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}
