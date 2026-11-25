package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// RecordedStorageTier is the storage tier the installation in dataDir runs:
// the one its enrollment configuration records. An installation that records
// none, or is no cluster member, runs the standard tier. A command reads it
// before it starts the database node; the enrollment bundle's own reader
// validates everything else in that file. A member that is still joining has
// no enrollment configuration yet and names its tier itself.
func RecordedStorageTier(dataDir string) (storagetier.Tier, error) {
	name, _, err := recordedStorageTierIn(filepath.Join(dataDir, "cluster-enrollment", "config.json"))
	if err != nil {
		return "", err
	}
	return storagetier.Parse(name)
}

func recordedStorageTierIn(path string) (string, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	var recorded struct {
		StorageTier string `json:"storage_tier"`
	}
	if err := json.NewDecoder(io.LimitReader(file, 128<<10)).Decode(&recorded); err != nil {
		return "", false, errors.New("read recorded storage tier: invalid cluster configuration")
	}
	return recorded.StorageTier, true, nil
}

// RecordStorageTier makes tier the storage tier the installation in dataDir
// records, and returns the one it recorded before. Nothing else in the
// enrollment configuration changes. The member's server reads the record when
// it starts, so the new timing applies from its next start; a cluster is moved
// to another timing one member at a time, each restarted and back with three
// voters before the next, and its members differ only for the length of that
// roll. Only a cluster member has the record.
func RecordStorageTier(dataDir string, tier storagetier.Tier) (storagetier.Tier, error) {
	if parsed, err := storagetier.Parse(string(tier)); err != nil || parsed != tier {
		return "", fmt.Errorf("storage tier %q is not one this build can run", tier)
	}
	dir := filepath.Join(dataDir, "cluster-enrollment")
	path := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("record storage tier: this installation is not a cluster member")
	}
	if err != nil {
		return "", err
	}
	var settings EnrollmentInfo
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return "", errors.New("record storage tier: invalid cluster configuration")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", errors.New("record storage tier: invalid trailing cluster configuration")
	}
	previous, err := storagetier.Parse(settings.StorageTier)
	if err != nil {
		return "", fmt.Errorf("record storage tier: the recorded tier %q is not one this build can run", settings.StorageTier)
	}
	if previous == tier {
		return previous, nil
	}
	settings.StorageTier = tier.Recorded()
	config, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	// Written beside the record and renamed over it, so a reader finds the
	// old configuration or the new one, never a part of either.
	stage, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(stage.Name())
	record, err := os.Stat(path)
	if err != nil {
		return "", errors.Join(err, stage.Close())
	}
	if err := likeRecord(stage, record); err != nil {
		return "", errors.Join(err, stage.Close())
	}
	_, writeErr := stage.Write(config)
	if err := errors.Join(writeErr, stage.Sync(), stage.Close()); err != nil {
		return "", err
	}
	if err := os.Rename(stage.Name(), path); err != nil {
		return "", err
	}
	folder, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	return previous, errors.Join(folder.Sync(), folder.Close())
}
