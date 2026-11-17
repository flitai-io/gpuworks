package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func episodeOn(nodeID uint, ident, check string, started time.Time, ended *time.Time) *CardHealthEpisode {
	return &CardHealthEpisode{NodeID: nodeID, CardIdent: ident, CheckID: check, Level: "fail", Reason: "RUNTIME_UNUSABLE",
		StartedAt: started, EndedAt: ended, LastSeenAt: started, Count: 1, FirstEvidence: "Detail=cuCtxCreate=999", LatestEvidence: "Detail=cuCtxCreate=999"}
}

// Episodes are inserted with an id and updated whole; open ones are read back
// on start; a node's page lists open ones first, then the newest.
func TestCardHealthEpisodesRoundTrip(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("n-health", HashToken("n-health"), "n-health")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ended := base.Add(time.Hour)
	older := episodeOn(node.ID, "uuid:GPU-1", "runtime", base, &ended)
	open := episodeOn(node.ID, "uuid:GPU-2", "pcie_link", base.Add(-time.Hour), nil)
	newest := episodeOn(node.ID, "uuid:GPU-1", "vendor_health", base.Add(2*time.Hour), &ended)
	if err := s.SaveCardHealthEpisodes([]*CardHealthEpisode{older, open, newest}); err != nil {
		t.Fatal(err)
	}
	if older.ID == 0 || open.ID == 0 || newest.ID == 0 {
		t.Fatalf("ids not assigned: %d %d %d", older.ID, open.ID, newest.ID)
	}
	open.Count, open.Flapping, open.LatestEvidence = 42, true, "PCIe Link 0000:3f:00.0=x8 of x16"
	if err := s.SaveCardHealthEpisodes([]*CardHealthEpisode{open}); err != nil {
		t.Fatal(err)
	}

	still, err := s.OpenCardHealthEpisodes()
	if err != nil || len(still) != 1 || still[0].ID != open.ID || still[0].Count != 42 || !still[0].Flapping ||
		still[0].LatestEvidence != "PCIe Link 0000:3f:00.0=x8 of x16" {
		t.Fatalf("open episodes = %+v, %v", still, err)
	}

	page, total, err := s.CardHealthEpisodes(CardHealthEpisodeQuery{NodeID: node.ID, Limit: 50})
	if err != nil || total != 3 || len(page) != 3 {
		t.Fatalf("page = %+v (%d), %v", page, total, err)
	}
	if page[0].ID != open.ID || page[1].ID != newest.ID || page[2].ID != older.ID {
		t.Fatalf("order = %d %d %d, want open, newest, older", page[0].ID, page[1].ID, page[2].ID)
	}
	byCard, total, err := s.CardHealthEpisodes(CardHealthEpisodeQuery{NodeID: node.ID, CardIdent: "uuid:GPU-1", CheckID: "runtime", Limit: 50})
	if err != nil || total != 1 || byCard[0].ID != older.ID {
		t.Fatalf("one card's one check = %+v (%d), %v", byCard, total, err)
	}
	second, total, err := s.CardHealthEpisodes(CardHealthEpisodeQuery{NodeID: node.ID, Offset: 2, Limit: 2})
	if err != nil || total != 3 || len(second) != 1 || second[0].ID != older.ID {
		t.Fatalf("second page = %+v (%d), %v", second, total, err)
	}
	recent, total, err := s.CardHealthEpisodes(CardHealthEpisodeQuery{NodeID: node.ID, StartedAfter: ended.Add(time.Minute), Limit: 50})
	if err != nil || total != 1 || recent[0].ID != open.ID {
		t.Fatalf("within the window = %+v (%d), %v", recent, total, err)
	}
}

// Old closed episodes are deleted a batch at a time; open ones never are.
func TestDeleteCardHealthEpisodesEndedBefore(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("n-prune", HashToken("n-prune"), "n-prune")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var rows []*CardHealthEpisode
	for i := 0; i < 5; i++ {
		end := old.Add(time.Duration(i) * time.Hour)
		rows = append(rows, episodeOn(node.ID, fmt.Sprintf("uuid:GPU-%d", i), "runtime", old, &end))
	}
	rows = append(rows, episodeOn(node.ID, "uuid:GPU-open", "runtime", old, nil))
	if err := s.SaveCardHealthEpisodes(rows); err != nil {
		t.Fatal(err)
	}
	cutoff := old.Add(30 * 24 * time.Hour)
	if n, err := s.DeleteCardHealthEpisodesEndedBefore(cutoff, 3); err != nil || n != 3 {
		t.Fatalf("first batch = %d, %v", n, err)
	}
	if n, err := s.DeleteCardHealthEpisodesEndedBefore(cutoff, 3); err != nil || n != 2 {
		t.Fatalf("second batch = %d, %v", n, err)
	}
	if _, total, _ := s.CardHealthEpisodes(CardHealthEpisodeQuery{NodeID: node.ID, Limit: 50}); total != 1 {
		t.Fatalf("left = %d, want the open one", total)
	}
}

// A deleted node takes its episodes along, and a write that raced the delete
// does not bring them back.
func TestADeletedNodesEpisodesGoWithIt(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("n-gone", HashToken("n-gone"), "n-gone")
	if err != nil {
		t.Fatal(err)
	}
	ep := episodeOn(node.ID, "uuid:GPU-1", "runtime", time.Now().UTC(), nil)
	if err := s.SaveCardHealthEpisodes([]*CardHealthEpisode{ep}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode("n-gone"); err != nil {
		t.Fatal(err)
	}
	ep.Count = 9
	late := episodeOn(node.ID, "uuid:GPU-2", "runtime", time.Now().UTC(), nil)
	if err := s.SaveCardHealthEpisodes([]*CardHealthEpisode{ep, late}); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := s.db.Model(&CardHealthEpisode{}).Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("episodes left = %d, %v", n, err)
	}
}

// A node's page is read through its index, not by sorting the whole table.
func TestCardHealthEpisodesUseTheNodeIndex(t *testing.T) {
	s := newTestStore(t)
	var plan []struct {
		Detail string
	}
	if err := s.db.Raw(`EXPLAIN QUERY PLAN SELECT * FROM card_health_episodes WHERE node_id = 1 ORDER BY ended_at IS NULL DESC, started_at DESC LIMIT 50`).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, p := range plan {
		details = append(details, p.Detail)
	}
	joined := strings.Join(details, "; ")
	if !strings.Contains(joined, "idx_card_health_node_started") {
		t.Fatalf("plan = %s, want the node index", joined)
	}
}

func TestCardHealthEpisodesMigration(t *testing.T) {
	s := newTestStore(t)
	m := migrationByName(t, "card_health_episodes")
	if !s.db.Migrator().HasTable(&CardHealthEpisode{}) || !s.db.Migrator().HasIndex(&CardHealthEpisode{}, "idx_card_health_node_started") {
		t.Fatal("table or index missing after migrations")
	}
	if err := m.Apply(s.db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	t.Run("rollback", func(t *testing.T) {
		failing := nodesOnlyDB(t, "")
		if err := failing.Exec(fmt.Sprintf(`CREATE TRIGGER fail_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(failing, []migration{m}); err == nil {
			t.Fatal("injected marker failure did not fail the migration")
		}
		if failing.Migrator().HasTable("card_health_episodes") {
			t.Fatal("the table survived a failed migration")
		}
		// Started again without the fault, it applies.
		if err := failing.Exec(`DROP TRIGGER fail_marker`).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(failing, []migration{m}); err != nil || !failing.Migrator().HasTable("card_health_episodes") {
			t.Fatalf("restart after rollback: %v", err)
		}
	})
}
