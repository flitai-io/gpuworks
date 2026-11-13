// Command gpuworks-pack-catalog validates and packs an inference catalog directory.
package main

import (
	"flag"
	"log"
	"os"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

func main() {
	catalog := flag.String("catalog", "", "catalog directory")
	output := flag.String("output", "", "output tar.gz path")
	flag.Parse()
	if *catalog == "" || *output == "" {
		log.Fatal("--catalog and --output are required")
	}
	fsys := os.DirFS(*catalog)
	if _, err := inferencecatalog.LoadPackageDirectory(*catalog); err != nil {
		log.Fatal(err)
	}
	file, err := os.Create(*output)
	if err != nil {
		log.Fatal(err)
	}
	if err := inferencecatalog.PackPackage(file, fsys); err != nil {
		_ = file.Close()
		_ = os.Remove(*output)
		log.Fatal(err)
	}
	if err := file.Close(); err != nil {
		log.Fatal(err)
	}
}
