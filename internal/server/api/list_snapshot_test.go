package api

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
)

// wholeListRead is a read of every deployment or every instance.
var wholeListRead = regexp.MustCompile("^SELECT \\* FROM [`\"]?(deployments|instances)[`\"]? ORDER BY id desc$")

// With the server's list snapshot on, the deployment and instance lists read
// every deployment and instance once between changes rather than on every
// request; what a page shows - an instance's pull, its message - is current
// all the same, and each page is exactly the page the per-request read gives.
func TestDeploymentAndInstanceListsShareTheSnapshot(t *testing.T) {
	f := newListFixture(t, 3)
	plain := &apiHandlers{d: Deps{Store: f.st}}
	f.handler.d.Lists = deployment.NewLists(f.st, nil, nil)
	var tableReads atomic.Int64
	if err := f.st.DB().Callback().Query().After("gorm:query").Register("test:list_table_reads", func(tx *gorm.DB) {
		if wholeListRead.MatchString(strings.TrimSpace(tx.Statement.SQL.String())) {
			tableReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.DB().Callback().Query().Remove("test:list_table_reads") })

	for _, path := range []string{"/api/v1/deployments?page=1&page_size=4", "/api/v1/instances?page=1&page_size=4"} {
		serve := f.handler.listDeployments
		plainServe := plain.listDeployments
		if path[8:17] == "instances" {
			serve, plainServe = f.handler.listInstances, plain.listInstances
		}
		f.get(t, serve, path)
		tableReads.Store(0)
		for range 3 {
			if got, want := f.get(t, serve, path), f.get(t, plainServe, path); string(got) != string(want) {
				t.Fatalf("%s from the snapshot differs from the per-request read:\n%s\nwant\n%s", path, got, want)
			}
		}
		// The per-request reads above read the tables 3 x 2 times; the snapshot none.
		if n := tableReads.Load(); n != 6 {
			t.Fatalf("%s: 3 unchanged requests read every deployment or instance %d times beyond the per-request path's 6", path, n-6)
		}
	}

	// An instance reports progress, which announces no change: the next page
	// shows it.
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "inst-extra-2", State: proto.InstRunning, Message: "serving 42 requests"}); err != nil {
		t.Fatal(err)
	}
	var page dto.ListPage[dto.Instance]
	if err := json.Unmarshal(f.get(t, f.handler.listInstances, "/api/v1/instances?page=1&page_size=50&q=inst-extra-2"), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Message != "serving 42 requests" {
		t.Fatalf("instance page = %+v, want the reported message", page.Items)
	}
}

// The dashboard counts the deployments by the snapshot's states: with nothing
// changed it reads no deployment or instance row, and its counts are those
// the per-request read gives. The list's state filter and state order read
// the same states.
func TestDashboardAndStateFilterReadTheSnapshotStates(t *testing.T) {
	f := newListFixture(t, 3)
	plain := &apiHandlers{d: Deps{Store: f.st}}
	f.handler.d.Lists = deployment.NewLists(f.st, nil, nil)
	var tableReads atomic.Int64
	if err := f.st.DB().Callback().Query().After("gorm:query").Register("test:dashboard_table_reads", func(tx *gorm.DB) {
		if wholeListRead.MatchString(strings.TrimSpace(tx.Statement.SQL.String())) {
			tableReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.DB().Callback().Query().Remove("test:dashboard_table_reads") })
	for _, path := range []string{"/api/v1/dashboard/summary", "/api/v1/deployments?page=1&page_size=50&state=running", "/api/v1/deployments?page=1&page_size=50&sort=-state"} {
		serve, plainServe := f.handler.dashboardSummary, plain.dashboardSummary
		if strings.Contains(path, "/deployments") {
			serve, plainServe = f.handler.listDeployments, plain.listDeployments
		}
		f.get(t, serve, path)
		want := f.get(t, plainServe, path)
		tableReads.Store(0)
		for range 3 {
			if got := f.get(t, serve, path); string(got) != string(want) {
				t.Fatalf("%s from the snapshot differs from the per-request read:\n%s\nwant\n%s", path, got, want)
			}
		}
		if n := tableReads.Load(); n != 0 {
			t.Fatalf("%s: 3 unchanged requests read every deployment or instance %d times, want 0", path, n)
		}
	}
}
