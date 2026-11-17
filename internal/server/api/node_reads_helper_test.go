package api

import (
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// wholeNodeRead is a read of every node's full row (store.ListNodes and its
// like): no WHERE, every column.
var wholeNodeRead = regexp.MustCompile("^SELECT \\* FROM [`\"]?nodes[`\"]?( ORDER BY [^ ]+( desc| asc)?)?$")

// countWholeNodeReads counts the whole-fleet node reads st serves from now on.
func countWholeNodeReads(t *testing.T, st *store.Store) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	name := "test:whole_node_reads:" + t.Name()
	if err := st.DB().Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if wholeNodeRead.MatchString(strings.TrimSpace(tx.Statement.SQL.String())) {
			n.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(name) })
	return &n
}
