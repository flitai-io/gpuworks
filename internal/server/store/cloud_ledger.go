package store

import cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"

// CloudLedger is the ZStack Cloud provider's ledger (its cloud hosts and
// resource leases, the tables of migrations 135 and 136) over this store's
// database. It is how the provider reaches those tables: the raw handle DB()
// returns is for tests, not for other packages.
func (s *Store) CloudLedger() *cloudstore.Store { return cloudstore.New(s.db) }
