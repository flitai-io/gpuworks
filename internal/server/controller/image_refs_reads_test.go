package controller

import (
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"
)

var wholeNodeReadSQL = regexp.MustCompile("^SELECT \\* FROM [`\"]?nodes[`\"]?( ORDER BY [^ ]+( desc| asc)?)?$")

// Handing the catalog's refs to every node needs each node's UUID only: not
// every node's full row and cards, read again on each catalog change.
func TestPushImageRefsReadsOnlyTheNodesUUIDs(t *testing.T) {
	st, hub, nodes := imageRefsFixture(t, "refs-uuids", 3)
	c := New(st, hub, nil)
	var reads atomic.Int64
	if err := st.DB().Callback().Query().After("gorm:query").Register("test:refs_node_reads", func(tx *gorm.DB) {
		if wholeNodeReadSQL.MatchString(strings.TrimSpace(tx.Statement.SQL.String())) {
			reads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove("test:refs_node_reads") })
	c.pushImageRefs()
	for _, uuid := range nodes {
		if got := hub.pushesTo(uuid); len(got) != 1 {
			t.Fatalf("node %s got %d refs pushes, want 1", uuid, len(got))
		}
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("pushing refs read every node's full row %d times, want 0", n)
	}
}
