package store

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/securefile"
)

// SnapshotTo writes a consistent copy of the live database to dest, which must
// not exist yet. It is how a server that keeps running takes the database
// into a pre-upgrade backup (ADR 0040); the copy needs no write-ahead log.
func (s *Store) SnapshotTo(dest string) error {
	if strings.TrimSpace(dest) == "" {
		return errors.New("snapshot needs a destination")
	}
	f, err := securefile.Create(dest)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("snapshot destination %s already exists", dest)
	}
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dest)
		return err
	}
	if err := s.db.Exec("VACUUM INTO ?", dest).Error; err != nil {
		_ = os.Remove(dest)
		return err
	}
	return securefile.Protect(dest)
}
