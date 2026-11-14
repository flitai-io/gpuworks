package deployment

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Rows resolves the rows a deployment or instance DTO joins in. A missing
// row is nil: the DTO simply leaves that field empty, exactly as it does when
// the store reports not-found.
type Rows interface {
	CachePhase(store.FileRef) string
	PreparationNodes(*store.Deployment) []uint
	ModelSource(uint, *store.Node, store.SourceRules) store.ModelSource
	ModelCopyObservation(*store.ModelFile) (proto.ModelFileStatus, bool)
	Instances(deploymentID uint) []store.Instance
	Deployment(id uint) *store.Deployment
	Node(id uint) *store.Node
	Model(id uint) *store.Model
	ModelFile(modelID, nodeID uint) *store.ModelFile
	FirstScheduleEvent(deploymentID uint) (store.InstanceEvent, bool)
	LatestScheduleEvent(deploymentID uint) (store.InstanceEvent, bool)
	// RetryExhausted reports whether the controller recorded that the unit
	// whose newest failed attempt this is gave up (ADR 0029).
	RetryExhausted(instanceUUID string) bool
	PendingTimeout() time.Duration
	// Requirements is the deployment's node Requirements; nil when it names
	// none or they cannot be read.
	Requirements(deploymentID uint) map[string][]string
	// ModelReads are the model files the instance reads.
	ModelReads(instanceID uint) []dto.InstanceModelFile
	// NodeByUUID is the node with that UUID; nil when there is none.
	NodeByUUID(uuid string) *store.Node
}

// StoreRows asks the store for each row as it is needed. It suits a single
// object; over a list it costs several queries per row.
type StoreRows struct {
	st *store.Store
	PreparationReads
}

// NewStoreRows asks st for each row, and prep for model preparation.
func NewStoreRows(st *store.Store, prep ModelPreparation) StoreRows {
	return StoreRows{st, NewPreparationReads(prep)}
}

func (l StoreRows) Instances(deploymentID uint) []store.Instance {
	insts, _ := l.st.InstancesForDeployment(deploymentID)
	return insts
}

func (l StoreRows) Deployment(id uint) *store.Deployment {
	d, err := l.st.GetDeployment(id)
	if err != nil {
		return nil
	}
	return d
}

func (l StoreRows) Node(id uint) *store.Node {
	n, err := l.st.NodeByID(id)
	if err != nil {
		return nil
	}
	return n
}

func (l StoreRows) Model(id uint) *store.Model {
	m, err := l.st.GetModel(id)
	if err != nil {
		return nil
	}
	return m
}

func (l StoreRows) ModelFile(modelID, nodeID uint) *store.ModelFile {
	mf, err := l.st.ModelFileByModelNode(modelID, nodeID)
	if err != nil {
		return nil
	}
	return mf
}

func (l StoreRows) FirstScheduleEvent(deploymentID uint) (store.InstanceEvent, bool) {
	ev, found, err := l.st.FirstScheduleEvent(deploymentID)
	return ev, found && err == nil
}

func (l StoreRows) RetryExhausted(instanceUUID string) bool {
	recorded, err := l.st.InstancesWithEvent(store.EventUnitRetryExhausted, []string{instanceUUID})
	return err == nil && recorded[instanceUUID]
}

func (l StoreRows) LatestScheduleEvent(deploymentID uint) (store.InstanceEvent, bool) {
	ev, found, err := l.st.LatestScheduleEvent(deploymentID)
	return ev, found && err == nil
}

func (l StoreRows) PendingTimeout() time.Duration {
	return l.st.DeploymentPolicy().PendingTimeout()
}

func (l StoreRows) Requirements(deploymentID uint) map[string][]string {
	reqs, _ := l.st.DeploymentRequirements(deploymentID)
	return reqs
}

func (l StoreRows) ModelReads(instanceID uint) []dto.InstanceModelFile {
	reads, err := l.st.InstanceModelFiles([]uint{instanceID})
	if err != nil {
		return nil
	}
	return instanceReadDTOs(reads[instanceID], func(id uint) *store.ModelFile {
		mf, err := l.st.ModelFileByID(id)
		if err != nil {
			return nil
		}
		return mf
	}, func(id uint) *store.Storage {
		st, err := l.st.StorageByID(id)
		if err != nil {
			return nil
		}
		return st
	})
}

func (l StoreRows) NodeByUUID(uuid string) *store.Node {
	n, err := l.st.NodeByUUID(uuid)
	if err != nil {
		return nil
	}
	return n
}

// instanceReadDTOs describes an instance's recorded reads with the storage
// each file is in.
func instanceReadDTOs(reads []store.InstanceModelFile, file func(uint) *store.ModelFile, storage func(uint) *store.Storage) []dto.InstanceModelFile {
	if len(reads) == 0 {
		return nil
	}
	out := make([]dto.InstanceModelFile, 0, len(reads))
	for _, read := range reads {
		item := dto.InstanceModelFile{ModelID: read.ModelID, Path: read.HostPath}
		if mf := file(read.ModelFileID); mf != nil {
			if st := storage(mf.StorageID); st != nil {
				item.StorageID, item.StorageName, item.StorageType = st.ID, st.Name, st.Type
			}
		}
		out = append(out, item)
	}
	return out
}

type modelNodeKey struct{ modelID, nodeID uint }

// ListRows holds everything a whole list joins against, read once. The query
// count is then independent of how many rows the list has.
type ListRows struct {
	storageRows           []store.Storage
	sources               *store.ModelSourceSnapshot
	instancesByDeployment map[uint][]store.Instance
	deployments           map[uint]*store.Deployment
	nodes                 map[uint]*store.Node
	models                map[uint]*store.Model
	modelFiles            map[modelNodeKey]*store.ModelFile
	scheduleEvents        map[uint]store.InstanceEvent
	// gaveUp is which failed instances carry the controller's give-up
	// record, read once for the whole list.
	gaveUp  map[string]bool
	reqs    map[uint]map[string][]string
	reads   map[uint][]dto.InstanceModelFile
	byUUID  map[string]*store.Node
	latest  map[uint]store.InstanceEvent
	timeout time.Duration
	// st is the store the list was read from.
	st *store.Store
	PreparationReads
	// deploymentRows and instanceRows are the rows the list was built from,
	// in their read order.
	deploymentRows []store.Deployment
	instanceRows   []store.Instance
	// shared marks rows a Lists snapshot hands to every reader: they are
	// read-only, and Fresh reads a page's progress again.
	shared bool
	// fresh holds what Fresh read again: the page's deployments' instances
	// (an absent deployment has none) and their models' files.
	fresh *freshRows
}

// freshRows is what Fresh read again for a page.
type freshRows struct {
	instances map[uint][]store.Instance
	files     map[modelNodeKey]*store.ModelFile
	models    map[uint]bool
	nodes     map[uint]bool
	sources   *store.ModelSourceSnapshot
}

// NewListRows reads everything a list of deployments and instances joins
// against, once.
func NewListRows(st *store.Store, prep ModelPreparation, deployments []store.Deployment, instances []store.Instance) (*ListRows, error) {
	nodes, err := st.NodeIdentities()
	if err != nil {
		return nil, err
	}
	return newListRows(st, prep, deployments, instances, nodes, false)
}

// newListRows is NewListRows over nodes already read. everyNode resolves
// model sources on every node rather than on the nodes preparing now (which
// the controller holds in memory and announces no change of): a snapshot
// kept across reads must answer for whichever node prepares later.
func newListRows(st *store.Store, prep ModelPreparation, deployments []store.Deployment, instances []store.Instance, nodes []store.Node, everyNode bool) (*ListRows, error) {
	l := &ListRows{
		deploymentRows:        deployments,
		instanceRows:          instances,
		st:                    st,
		PreparationReads:      PreparationReads{prep},
		instancesByDeployment: map[uint][]store.Instance{},
		deployments:           make(map[uint]*store.Deployment, len(deployments)),
		nodes:                 map[uint]*store.Node{},
		models:                map[uint]*store.Model{},
		modelFiles:            map[modelNodeKey]*store.ModelFile{},
		timeout:               st.DeploymentPolicy().PendingTimeout(),
	}
	for i := range deployments {
		l.deployments[deployments[i].ID] = &deployments[i]
	}
	// ListInstances and InstancesForDeployment share an order, so grouping
	// keeps each deployment's instances in the order the single path returns.
	for _, inst := range instances {
		l.instancesByDeployment[inst.DeploymentID] = append(l.instancesByDeployment[inst.DeploymentID], inst)
	}
	l.byUUID = make(map[string]*store.Node, len(nodes))
	for i := range nodes {
		l.nodes[nodes[i].ID] = &nodes[i]
		l.byUUID[nodes[i].UUID] = &nodes[i]
	}
	models, err := st.ListModels()
	if err != nil {
		return nil, err
	}
	for i := range models {
		l.models[models[i].ID] = &models[i]
	}
	modelFiles, err := st.ListModelFiles()
	if err != nil {
		return nil, err
	}
	fileByID := make(map[uint]*store.ModelFile, len(modelFiles))
	for i := range modelFiles {
		fileByID[modelFiles[i].ID] = &modelFiles[i]
		key := modelNodeKey{modelFiles[i].ModelID, modelFiles[i].NodeID}
		if _, seen := l.modelFiles[key]; !seen {
			l.modelFiles[key] = &modelFiles[i]
		}
	}
	if err := l.loadReads(st, instances, fileByID); err != nil {
		return nil, err
	}
	if l.scheduleEvents, err = st.FirstScheduleEvents(); err != nil {
		return nil, err
	}
	var failed []string
	for _, inst := range instances {
		if inst.State == proto.InstFailed {
			failed = append(failed, inst.UUID)
		}
	}
	if l.gaveUp, err = st.InstancesWithEvent(store.EventUnitRetryExhausted, failed); err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(deployments))
	for i := range deployments {
		ids = append(ids, deployments[i].ID)
	}
	if l.latest, err = st.LatestScheduleEvents(); err != nil {
		return nil, err
	}
	all, err := st.DeploymentRequirementsFor(ids)
	if err != nil {
		return nil, err
	}
	l.reqs = make(map[uint]map[string][]string, len(all))
	for id, reqs := range all {
		l.reqs[id] = reqs
	}
	if err := l.loadPreparationSources(deployments, modelFiles, everyNode); err != nil {
		return nil, err
	}
	return l, nil
}

// loadReads reads every listed instance's model files once.
func (l *ListRows) loadReads(st *store.Store, instances []store.Instance, fileByID map[uint]*store.ModelFile) error {
	ids := make([]uint, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.ID)
	}
	reads, err := st.InstanceModelFiles(ids)
	if err != nil {
		return err
	}
	storages, err := st.ListStorages()
	if err != nil {
		return err
	}
	l.storageRows = storages
	storageByID := make(map[uint]*store.Storage, len(storages))
	for i := range storages {
		storageByID[storages[i].ID] = &storages[i]
	}
	l.reads = make(map[uint][]dto.InstanceModelFile, len(reads))
	for id, rows := range reads {
		l.reads[id] = instanceReadDTOs(rows, func(id uint) *store.ModelFile { return fileByID[id] }, func(id uint) *store.Storage { return storageByID[id] })
	}
	return nil
}

func (l *ListRows) ModelReads(instanceID uint) []dto.InstanceModelFile { return l.reads[instanceID] }

// NodeByUUID is built with the rows, never on first use: a snapshot's rows
// are read by many requests at once.
func (l *ListRows) NodeByUUID(uuid string) *store.Node { return l.byUUID[uuid] }

func (l *ListRows) Instances(deploymentID uint) []store.Instance {
	if l.fresh != nil {
		if insts, ok := l.fresh.instances[deploymentID]; ok {
			return insts
		}
	}
	return l.instancesByDeployment[deploymentID]
}

func (l *ListRows) Deployment(id uint) *store.Deployment { return l.deployments[id] }

func (l *ListRows) Node(id uint) *store.Node { return l.nodes[id] }

func (l *ListRows) Model(id uint) *store.Model { return l.models[id] }

func (l *ListRows) ModelFile(modelID, nodeID uint) *store.ModelFile {
	if l.fresh != nil && l.fresh.models[modelID] && l.fresh.nodes[nodeID] {
		return l.fresh.files[modelNodeKey{modelID, nodeID}]
	}
	return l.modelFiles[modelNodeKey{modelID, nodeID}]
}

func (l *ListRows) FirstScheduleEvent(deploymentID uint) (store.InstanceEvent, bool) {
	ev, found := l.scheduleEvents[deploymentID]
	return ev, found
}

func (l *ListRows) PendingTimeout() time.Duration { return l.timeout }

func (l *ListRows) LatestScheduleEvent(deploymentID uint) (store.InstanceEvent, bool) {
	ev, found := l.latest[deploymentID]
	return ev, found
}

func (l *ListRows) RetryExhausted(instanceUUID string) bool { return l.gaveUp[instanceUUID] }

func (l *ListRows) Requirements(deploymentID uint) map[string][]string {
	return l.reqs[deploymentID]
}
