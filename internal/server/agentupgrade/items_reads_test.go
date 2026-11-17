package agentupgrade

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"
)

var wholeNodeReadSQL = regexp.MustCompile("^SELECT \\* FROM [`\"]?nodes[`\"]?( ORDER BY [^ ]+( desc| asc)?)?$")

// A run's items show each node's current agent version, read from the nodes'
// upgrade columns rather than every node's full row and cards.
func TestItemsReadOnlyTheAgentVersions(t *testing.T) {
	f := newFixture(t, 3, true)
	r := f.create(t, "items", nil, 0)
	var reads atomic.Int64
	if err := f.st.DB().Callback().Query().After("gorm:query").Register("test:items_node_reads", func(tx *gorm.DB) {
		if wholeNodeReadSQL.MatchString(strings.TrimSpace(tx.Statement.SQL.String())) {
			reads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.DB().Callback().Query().Remove("test:items_node_reads") })
	page, err := f.s.Items(context.Background(), r.Record.ID, 1, 50, "")
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("items = %+v err %v", page, err)
	}
	for _, item := range page.Items {
		if item.CurrentVersion != "old" {
			t.Fatalf("item %s current version = %q, want old", item.Record.NodeUUID, item.CurrentVersion)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("listing items read every node's full row %d times, want 0", n)
	}
}
