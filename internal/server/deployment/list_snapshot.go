package deployment

import (
	"slices"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// listsMaxAge is how long a Lists snapshot is kept with no change announced:
// a write that announces nothing (another process, a direct database write)
// is on a list this late at most.
const listsMaxAge = 30 * time.Second

// listTopics are the bus topics whose changes a list's rows are read from.
// Progress (a transfer's bytes, an engine's message) counts on none of them;
// Fresh reads it again for the rows a page shows.
var listTopics = []string{"deployment", "instance", "model", "modelfile", "storage", "setting"}

// Lists keeps one ListRows of every deployment and instance for the
// deployment and instance lists, instead of each request reading every row
// and every join again.
//
// A read rebuilds the snapshot when a change it is read from has been
// announced since it was built: a non-progress event of listTopics, a
// scheduling event, a node's identity, state or labels. A deployment created,
// stopped or deleted is therefore on the very next page. A write nothing
// announces is seen within listsMaxAge.
//
// Whole-list facts - which rows match a filter, their order, their states -
// come from the snapshot; the rows a page shows are read again with Fresh,
// so their progress is current.
type Lists struct {
	st   *store.Store
	prep ModelPreparation
	now  func() time.Time

	mu      sync.Mutex
	rows    *ListRows
	started time.Time
	marks   []uint64
}

// NewLists keeps the lists of st; prep is the model preparation the rows
// observe, now the clock (nil is time.Now).
func NewLists(st *store.Store, prep ModelPreparation, now func() time.Time) *Lists {
	if now == nil {
		now = time.Now
	}
	return &Lists{st: st, prep: prep, now: now}
}

// ReadListRows reads every deployment and instance and what they join
// against for one request, the read Lists keeps between changes.
func ReadListRows(st *store.Store, prep ModelPreparation) (*ListRows, error) {
	nodes, err := st.NodeIdentities()
	if err != nil {
		return nil, err
	}
	return readListRows(st, prep, nodes, false)
}

// readListRows reads every deployment and instance and builds their rows over
// nodes.
func readListRows(st *store.Store, prep ModelPreparation, nodes []store.Node, everyNode bool) (*ListRows, error) {
	deps, err := st.ListDeployments()
	if err != nil {
		return nil, err
	}
	insts, err := st.ListInstances()
	if err != nil {
		return nil, err
	}
	return newListRows(st, prep, deps, insts, nodes, everyNode)
}

// Rows is the snapshot of every deployment and instance, rebuilt first when
// it is stale. The rows are shared by every reader and must not be changed;
// Deployments and AllInstances hand out copies.
func (l *Lists) Rows() (*ListRows, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	nodes, err := l.st.NodeIdentities()
	if err != nil {
		return nil, err
	}
	marks := l.marks0()
	if l.rows != nil && now.Sub(l.started) < listsMaxAge && slices.Equal(marks, l.marks) && sameNodeIdentities(nodes, l.rows) {
		return l.rows, nil
	}
	// The marks were taken before the rows are read: a change landing while
	// they are read makes the next read rebuild again.
	rows, err := readListRows(l.st, l.prep, nodes, true)
	if err != nil {
		return nil, err
	}
	rows.shared = true
	l.rows, l.started, l.marks = rows, now, marks
	return rows, nil
}

// marks0 are the counts a snapshot is current against.
func (l *Lists) marks0() []uint64 {
	marks := make([]uint64, 0, len(listTopics)+1)
	for _, topic := range listTopics {
		var g uint64
		if l.st.Bus != nil {
			g = l.st.Bus.Generation(topic)
		}
		marks = append(marks, g)
	}
	return append(marks, l.st.ScheduleEventCount())
}

// sameNodeIdentities reports whether nodes are the ones rows were built
// from: a node's routine status report can change its hostname or state
// without a node change being announced, and the lists show both.
func sameNodeIdentities(nodes []store.Node, rows *ListRows) bool {
	if len(nodes) != len(rows.nodes) {
		return false
	}
	for i := range nodes {
		n := &nodes[i]
		was := rows.nodes[n.ID]
		if was == nil || was.UUID != n.UUID || was.Name != n.Name || was.Hostname != n.Hostname ||
			was.State != n.State || was.Labels != n.Labels || was.RecoveryPhase != n.RecoveryPhase {
			return false
		}
	}
	return true
}

// Deployments is a copy of the deployments the rows were built from, newest
// first.
func (l *ListRows) Deployments() []store.Deployment { return slices.Clone(l.deploymentRows) }

// AllInstances is a copy of the instances the rows were built from, newest
// first.
func (l *ListRows) AllInstances() []store.Instance { return slices.Clone(l.instanceRows) }

// Fresh is the rows with the given deployments' instances and their models'
// files read again: what a page shows of rows whose progress (bytes,
// messages, observations) changes without a change being announced. Rows
// built for one request are already current and are returned as they are.
func (l *ListRows) Fresh(deploymentIDs []uint) (*ListRows, error) {
	if !l.shared || len(deploymentIDs) == 0 {
		return l, nil
	}
	insts, err := l.st.InstancesForDeployments(deploymentIDs)
	if err != nil {
		return nil, err
	}
	fresh := &freshRows{instances: make(map[uint][]store.Instance, len(deploymentIDs)), files: map[modelNodeKey]*store.ModelFile{}, models: map[uint]bool{}, nodes: map[uint]bool{0: true}}
	for _, inst := range insts {
		fresh.instances[inst.DeploymentID] = append(fresh.instances[inst.DeploymentID], inst)
	}
	// The files a page shows are its models' on the nodes it is prepared on
	// and its instances run on, and their shared rows (node 0).
	var modelIDs []uint
	nodeIDs := []uint{0}
	addNode := func(id uint) {
		if !fresh.nodes[id] {
			fresh.nodes[id] = true
			nodeIDs = append(nodeIDs, id)
		}
	}
	for _, id := range deploymentIDs {
		if _, ok := fresh.instances[id]; !ok {
			fresh.instances[id] = nil
		}
		dep := l.deployments[id]
		if dep == nil {
			continue
		}
		ids, err := planning.DeploymentModelIDs(dep)
		if err != nil {
			ids = []uint{dep.ModelID}
		}
		for _, modelID := range ids {
			if modelID != 0 && !fresh.models[modelID] {
				fresh.models[modelID] = true
				modelIDs = append(modelIDs, modelID)
			}
		}
		for _, nodeID := range l.preparationNodeIDs(dep, fresh.instances[id]) {
			addNode(nodeID)
		}
		for _, inst := range fresh.instances[id] {
			addNode(inst.NodeID)
		}
	}
	files, err := l.st.ModelFilesForModelsOnNodes(modelIDs, nodeIDs)
	if err != nil {
		return nil, err
	}
	for i := range files {
		key := modelNodeKey{files[i].ModelID, files[i].NodeID}
		if _, seen := fresh.files[key]; !seen {
			fresh.files[key] = &files[i]
		}
	}
	fresh.sources = l.sources.WithModelFiles(modelIDs, nodeIDs, files)
	out := *l
	out.fresh = fresh
	return &out, nil
}
