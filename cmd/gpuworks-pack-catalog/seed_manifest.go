package main

import (
	"flag"
	"fmt"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

func writeSeedManifest(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("seed-manifest", flag.ContinueOnError)
	date := flags.String("published-at", "", "catalog publication time (RFC3339); omit for an unpublished build")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: gpuworks-pack-catalog seed-manifest [--published-at TIMESTAMP]")
	}
	var publishedAt time.Time
	if *date != "" {
		parsed, err := time.Parse(time.RFC3339, *date)
		if err != nil {
			return fmt.Errorf("seed publication time: %w", err)
		}
		publishedAt = parsed
	}
	data, err := seed.BuildManifest(publishedAt)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}
