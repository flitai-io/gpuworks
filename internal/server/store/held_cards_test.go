package store

import (
	"strings"
	"testing"

	"gorm.io/gorm"
)

// The instance list's notes read the held cards of the page's nodes only,
// through the node index: one bounded query per page, never a scan of every
// card in the fleet.
func TestHeldCardsOnNodesReadsOnlyThoseNodesHeldCards(t *testing.T) {
	s := newTestStore(t)
	var nodes []*Node
	for _, uuid := range []string{"a", "b", "c"} {
		node, err := s.RegisterNode(uuid, HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	held := `[{"pid":1,"name":"python3","memory_bytes":1073741824}]`
	for _, card := range []GPU{
		{NodeID: nodes[0].ID, Index: 0, Ident: "a0", ForeignProcesses: held},
		{NodeID: nodes[0].ID, Index: 1, Ident: "a1"},
		{NodeID: nodes[1].ID, Index: 0, Ident: "b0", ForeignProcesses: held},
		{NodeID: nodes[2].ID, Index: 3, Ident: "c3", ForeignProcesses: held},
	} {
		if err := s.db.Create(&card).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.HeldCardsOnNodes([]uint{nodes[0].ID, nodes[2].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("held cards = %+v, want a0 and c3", got)
	}
	for _, card := range got {
		if card.ForeignProcesses == "" || card.NodeID == nodes[1].ID {
			t.Fatalf("card %+v", card)
		}
	}
	if none, err := s.HeldCardsOnNodes(nil); err != nil || len(none) != 0 {
		t.Fatalf("no nodes = %+v %v", none, err)
	}
	stmt := heldCardsQuery(s.db, []uint{1, 2}).Session(&gorm.Session{DryRun: true}).Find(&[]GPU{}).Statement
	var plan []struct{ Detail string }
	if err := s.db.Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	var text []string
	for _, row := range plan {
		text = append(text, row.Detail)
	}
	if joined := strings.Join(text, " | "); !strings.Contains(joined, "USING INDEX") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("held cards query plan = %s, want an index range", joined)
	}
}
