package api

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) modelDTO(m *store.Model, withNodes bool) dto.Model {
	d := dto.Model{
		ID: m.ID, Source: m.Source, SourceID: m.SourceID, SourceURL: m.SourceURL, Revision: m.Revision, ArtifactSubfolder: m.ArtifactSubfolder,
		Name: m.Name, Arch: m.Arch, QuantLabel: m.QuantLabel,
		WeightGiB: float64(m.WeightBytes) / (1 << 30),
		CreatedAt: utcTime(m.CreatedAt),
		README:    m.README, LogoPath: m.LogoPath,
		OriginCreatedAt: utcTimePtr(m.OriginCreatedAt), OriginUpdatedAt: utcTimePtr(m.OriginUpdatedAt),
		ContextLength: m.ContextLength, Precision: m.Precision,
		SourceIDEditable: store.ModelSourceIDEditable(m),
	}
	_ = json.Unmarshal([]byte(m.Operations), &d.Operations)
	_ = json.Unmarshal([]byte(m.InputModalities), &d.InputModalities)
	if d.Operations == nil {
		d.Operations = []inferenceapi.Operation{}
	}
	if d.InputModalities == nil {
		d.InputModalities = []string{}
	}
	if withNodes {
		a.fillModelPresence(&d, m.ID)
	}
	return d
}

// modelDTOs maps a list of models with where each one's files are, reading
// the presence facts once for the whole list.
func (a *apiHandlers) modelDTOs(models []store.Model) []dto.Model {
	lookup := a.newPresenceLookup(nil)
	out := make([]dto.Model, 0, len(models))
	for i := range models {
		d := a.modelDTO(&models[i], false)
		lookup.fill(&d, models[i].ID)
		out = append(out, d)
	}
	return out
}

func (a *apiHandlers) deploymentDTO(d *store.Deployment, withInstances bool) dto.Deployment {
	return a.deploymentDTOFrom(storeLookup{a.d.Store, preparationReads{a.d.Preparation}}, d, withInstances)
}

// deploymentDTOFrom builds the DTO from whichever lookup the caller holds: the
// store for one deployment, a preloaded set for a list.
func (a *apiHandlers) deploymentDTOFrom(rows dtoLookup, d *store.Deployment, withInstances bool) dto.Deployment {
	policy := deploy.NormalizeRoutingPolicy(d.RoutingPolicy)
	out := dto.Deployment{
		ID: d.ID, TemplateID: d.TemplateID, TemplateSpecSource: d.TemplateSpecSource, TemplateSpecName: d.TemplateSpecName,
		TemplateScenario: d.TemplateScenario, TemplateDeviated: d.TemplateDeviated,
		Name: d.Name, Engine: d.Engine, Image: d.Image, ModelID: d.ModelID, ServedName: d.ServedName,
		DesiredState: d.State.String(), Sleepable: d.Sleepable, PartitionBackend: d.PartitionBackend, VRAMLimitBytes: d.VRAMLimitBytes, Replicas: d.Replicas, MaxReplicas: planning.MaxReplicas, CreatedAt: utcTime(d.CreatedAt),
		DistGroup: d.DistGroup, DistRole: d.DistRole, NNodes: d.NNodes, NodeRank: d.NodeRank,
		MasterAddr: d.MasterAddr, MasterPort: d.MasterPort,
		RoutingPolicy: policy, StartTimeoutSec: d.StartTimeoutSec, RestartRequestedAt: utcTimePtr(d.RestartRequestedAt),
	}
	out.ExtraArgs = d.ExtraArgs
	out.Health = a.deploymentHealth(d)
	out.TokenizeEnabled = !d.TokenizeDisabled
	out.YieldPolicy = dto.YieldPolicyOf(d.Policy())
	out.WakeWaitLimitSec = int(automation.DefaultWakeWaitLimit / time.Second)
	out.YieldPolicyEligible = dto.EligibilityOf(planning.YieldPolicyRefusal(d))
	out.ScheduleCapabilities = scheduleCapabilitiesDTO(planning.ScheduleCapabilitiesOf(d))
	out.NodeReplacement = dto.NodeReplacement{AfterSec: d.NodeReplaceAfterSec}
	out.PlacementMode = unitsPlacementMode(d)
	if strings.TrimSpace(d.QuickParams) != "" {
		_ = json.Unmarshal([]byte(d.QuickParams), &out.QuickParams)
	}
	if d.CatalogSelectionSnapshot != "" {
		// The typed reader answers "what did this deployment select"; a row it
		// cannot type (a field with an impossible value, written by hand or by a
		// version that changed a type) still reaches the client as it was
		// stored, because the response reports the row rather than judging it.
		if selection, err := deploy.DecodeCatalogSelection([]byte(d.CatalogSelectionSnapshot)); err == nil {
			out.MatchedBy = selection.MatchedBy
			encoded, encodeErr := selection.Encode()
			if encodeErr != nil {
				encoded = []byte(d.CatalogSelectionSnapshot)
			}
			var document map[string]any
			if json.Unmarshal(encoded, &document) == nil {
				out.CatalogSelectionSnapshot = document
			}
		} else {
			_ = json.Unmarshal([]byte(d.CatalogSelectionSnapshot), &out.CatalogSelectionSnapshot)
		}
		redactAcquisitionSecrets(out.CatalogSelectionSnapshot)
	}
	insts := rows.instances(d.ID)
	for i := range insts {
		if since := insts[i].WakeWaitingSince; since != nil && (out.WakeWaitingSince == nil || since.Before(*out.WakeWaitingSince)) {
			out.WakeWaitingSince = utcTimePtr(since)
		}
	}
	// One match and one walk serve every replica field of the response:
	// ready_replicas and asleep_replicas are two counts of the same replicas,
	// judged by the predicate the gateway routes on, so no field of a row can
	// disagree with another or with what the gateway answers.
	view, viewErr := newReplicaView(rows, d, insts)
	var facts deploymentFacts
	if viewErr == nil {
		facts = deploymentFactsOf(rows, view, insts)
		out.PD = deploymentPDDTO(rows, d, insts, view, facts)
		// ready_replicas answers the same question for a PD deployment as for
		// an ordinary one, and the drawer pairs it with asleep_replicas, so it
		// is filled from the same walk either way. Leaving it off a PD response
		// was what made the drawer show a numerator with no denominator.
		ready := facts.Ready
		out.ReadyReplicas = &ready
		if out.PD != nil {
			// The stored Replicas only configures an ordinary deployment (PD
			// creation forces it to 1), while asleep_replicas counts PD units
			// across roles. Rewriting it to the role total here is the same
			// rewrite the list row makes, so the drawer's denominator and the
			// table's describe the same replicas instead of printing "2/1".
			desired := 0
			for _, role := range out.PD.Roles {
				desired += role.Desired
			}
			out.Replicas = desired
		}
		if out.PD == nil {
			out.RetryExhausted = ordinaryRetryExhausted(rows, view)
			out.PlacementStatus = placementStatus(rows, d, view)
			// Listed only while the stored node list reads: an unreadable
			// one is reported as no list, as node_uuids is.
			if ids, err := d.StoredNodeIDs(); d.NNodes > 1 && err == nil && len(ids) > 0 {
				out.Units = replicaUnitsDTO(rows, d, view)
			}
		}
	}
	out.Operations = []dto.DeploymentOperation{}
	model := rows.model(d.ModelID)
	contract := inferenceapi.ResolveDeploymentContract(nil, d) // no operations
	if model != nil && d.ModelID != 0 {
		contract = inferenceapi.ModelContract(model)
	}
	for _, definition := range inferenceapi.Definitions() {
		for _, operation := range contract.Operations {
			if operation.Type == definition.Type {
				out.Operations = append(out.Operations, dto.DeploymentOperation{Type: operation.Type, Endpoint: operation.Endpoint, Renderer: definition.Renderer})
			}
		}
	}
	// An unreadable list is reported as no list.
	storedNodeIDs, _ := d.StoredNodeIDs()
	for _, id := range storedNodeIDs {
		if n := rows.node(id); n != nil {
			out.NodeUUIDs = append(out.NodeUUIDs, n.UUID)
			out.NodeNames = append(out.NodeNames, nodeDisplayName(n))
		}
	}
	if model != nil {
		out.ModelName = model.Name
	}
	out.NodeRequirements = rows.requirements(d.ID)
	if out.NodeRequirements == nil {
		out.NodeRequirements = map[string][]string{}
	}
	out.PlacementStrategy = string(d.PlacementStrategy)
	primaryNode := rows.node(d.NodeID)
	if primaryNode != nil {
		out.NodeUUID = primaryNode.UUID
	}
	progress, recorded := deploymentModelProgress(rows, d)
	out.ModelProgress = progress
	if primaryNode != nil {
		out.NodeName = nodeDisplayName(primaryNode)
	}
	out.RuntimeNodes, out.RuntimeNodeNames = deploymentRuntimeNodes(rows, insts)
	out.RuntimeState = deploymentRuntimeState(rows, d, insts, recorded)
	out.SleepLevel = facts.Level
	out.AsleepReplicas = facts.AsleepReplicas
	out.SleepHold = facts.SleepHold()
	out.SleepHeld = out.SleepHold == dto.SleepHoldAll
	out.SleepWakesOnRequest = planning.OperatorSleepWakesOnRequest(d)
	out.State = effectiveDeploymentState(d, out.RuntimeState, facts)
	if withInstances {
		out.NodeRequirementLabels = a.requirementLabels(out.NodeRequirements)
		for i := range insts {
			item := a.instanceDTOFrom(rows, &insts[i])
			if viewErr == nil {
				replicaFields(&item, d, view.match, &insts[i])
			}
			out.Instances = append(out.Instances, item)
		}
	}
	return out
}

// instanceRecordsPresence reports whether an instance row still means the
// deployment is on that node. Every state but stopped does: a stopped row
// records that its container is gone and only waits to be deleted, so a
// replica that moved away must not keep its old node on the list.
//
// Failed counts, and it is the case that matters most: a replica that failed
// on a node ran there, holds that node's GPUs until the controller retires it,
// and is where an operator goes for its logs. Reporting only the node the
// deployment was still targeting at the time is what sent operators to the
// wrong machine.
func instanceRecordsPresence(state string) bool {
	return state != proto.InstStopped
}

// deploymentRuntimeNodes is where the deployment's instances are, node by
// node: one entry per node with how many instance rows it holds, and the same
// names in the same order. The rows are the authority - the deployment's
// placement target says where it was asked to run, and automatic placement,
// retries and spreading all move instances away from it.
//
// An instance whose node row is missing is left out: it names no node a client
// could show. Deleting a node requires that no instance still runs on it
// (store.DeleteNode), so a listed instance nearly always resolves.
//
// The order is stable and independent of the instance order in the store, so
// the same fleet answers the same bytes whatever order the rows came back in.
func deploymentRuntimeNodes(rows dtoLookup, insts []store.Instance) ([]dto.DeploymentRuntimeNode, []string) {
	if len(insts) == 0 {
		return nil, nil
	}
	counts := map[uint]int{}
	var order []uint
	for _, instance := range insts {
		if !instanceRecordsPresence(instance.State) {
			continue
		}
		if _, seen := counts[instance.NodeID]; !seen {
			order = append(order, instance.NodeID)
		}
		counts[instance.NodeID]++
	}
	out := make([]dto.DeploymentRuntimeNode, 0, len(order))
	for _, nodeID := range order {
		node := rows.node(nodeID)
		if node == nil {
			continue
		}
		out = append(out, dto.DeploymentRuntimeNode{
			NodeUUID: node.UUID, NodeName: nodeDisplayName(node), Instances: counts[nodeID],
		})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].NodeName != out[b].NodeName {
			return out[a].NodeName < out[b].NodeName
		}
		return out[a].NodeUUID < out[b].NodeUUID
	})
	if len(out) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(out))
	for _, node := range out {
		names = append(names, node.NodeName)
	}
	return out, names
}

// deploymentRuntimeState is the runtime_state the deployment DTO reports.
func deploymentRuntimeState(rows dtoLookup, d *store.Deployment, insts []store.Instance, progress []dto.ModelNodePresence) string {
	state := deriveRuntimeState(d.State, insts, progress, scheduleStalled(rows, d.ID, len(insts)))
	if state != "running" {
		return state
	}
	connected := onlineInstances(rows, insts)
	incomplete := planning.GroupsIncomplete(connected)
	for _, inst := range connected {
		// Sleeping engines retain their runtime state; sleep is presented separately.
		if inst.State == proto.InstRunning && !inst.Headless && !incomplete[inst.UUID] {
			return state
		}
	}
	for _, inst := range insts {
		if inst.State == proto.InstRunning && !instanceNodeOnline(rows, inst.NodeID) {
			return "offline"
		}
	}
	return state
}

// effectiveDeploymentState is the one state a list shows for a deployment: a
// delete in flight outranks whatever the runtime reports, as in the UI's
// effectiveDeploymentState, and a deployment the gateway refuses every request
// of because the replicas it needs sleep is asleep (or sleeping, while the
// order is still being carried out) rather than the running state its engines
// keep reporting.
//
// A partly slept deployment keeps the runtime state: its awake replicas serve,
// and asleep_replicas says how many do not, so the row shows both instead of
// passing itself off as entirely well or entirely asleep.
func effectiveDeploymentState(d *store.Deployment, runtimeState string, facts deploymentFacts) string {
	if d.State == store.DeployDeleting {
		return deploymentStateDeleting
	}
	if runtimeState == "running" && facts.Asleep() {
		return facts.State()
	}
	return runtimeState
}

const deploymentStateDeleting = "deleting"

// deploymentListStates are the states a deployment list can be filtered by.
var deploymentListStates = []string{"pending", "pulling", "starting", "running", "offline", "failed", "stopped", deploymentStateDeleting, deploymentStateSleeping, deploymentStateAsleep}

// scheduleStalled reports whether a deployment with no instance has been
// refused a placement for longer than the configured timeout.
//
// It measures from the FIRST scheduling entry, not the latest: entries are
// rewritten once per timeout window, so the newest one is never older than a
// window and would report "not stalled" forever.
//
// It also does not measure from deployment creation. A 66 GiB download
// legitimately takes tens of minutes, and counting from creation would call a
// perfectly healthy download a stall - the same confusion this whole mechanism
// exists to remove.
func scheduleStalled(rows dtoLookup, deploymentID uint, instanceCount int) bool {
	if instanceCount > 0 {
		return false
	}
	ev, found := rows.firstScheduleEvent(deploymentID)
	if !found {
		return false
	}
	return time.Since(ev.CreatedAt) >= rows.pendingTimeout()
}

// modelsReady reports whether there is no download left to wait for.
//
// `present` and `registered` are the two ready states, exactly as the download
// reconciliation treats them (store.deploy: "current.State == MFPresent ||
// current.State == MFRegistered"). `registered` is what a locally registered
// path carries, and a deployment of one is ready the moment it is created.
//
// An empty list is ready too: with no model files recorded there is nothing
// outstanding, so a scheduling refusal is the whole story.
func modelsReady(modelProgress []dto.ModelNodePresence) bool {
	for _, mp := range modelProgress {
		switch mp.State {
		case proto.MFPresent, proto.MFRegistered:
		default:
			return false
		}
	}
	return true
}

func deriveRuntimeState(desired store.DeploymentState, instances []store.Instance, modelProgress []dto.ModelNodePresence, scheduleStalled bool) string {
	if desired == store.DeployStopped || desired == store.DeployDeleting {
		return "stopped"
	}
	if len(instances) == 0 {
		// No instance can exist yet until every required model file is
		// present on the target node. Failed downloads, insufficient disk,
		// and required downloads need operator action before they can proceed
		// (free disk space, start a public download or retry peer sync,
		// internal/server/controller/modelsync.go) — so without this check a
		// permanently-blocked deployment looks identical to one that is
		// merely queued and about to schedule normally.
		for _, mp := range modelProgress {
			if mp.State == proto.MFFailed || mp.State == proto.MFDownloadRequired || mp.State == proto.MFInsufficientDisk {
				return "failed"
			}
		}
		// The scheduler has explicitly refused a placement and kept refusing for
		// longer than the configured window. That is not "queued and about to
		// schedule", and reporting it as pending is what left deployments
		// sitting forever with no visible reason.
		//
		// Only while the weights are actually present, though. Scheduling is
		// not even attempted until every model file is in place
		// (controller.ensureDeploymentModelsPresent returns early otherwise), so
		// a refusal recorded earlier says nothing about a deployment that is
		// downloading right now.
		if scheduleStalled && modelsReady(modelProgress) {
			return "failed"
		}
		return "pending"
	}
	// Failed/stopped rows are attempt history once a replacement instance is
	// active. Retries deliberately use a fresh instance UUID and keep terminal
	// rows for diagnostics, so an old failure must not hide a currently pulling,
	// starting, or running replacement.
	hasLive, hasFailed := false, false
	for _, instance := range instances {
		switch instance.State {
		case proto.InstFailed:
			hasFailed = true
		case proto.InstStopped, proto.InstPendingRemoval:
		default:
			hasLive = true
		}
	}
	if !hasLive && hasFailed {
		return "failed"
	}
	for _, instance := range instances {
		if instance.State == proto.InstPulling {
			return "pulling"
		}
	}
	for _, instance := range instances {
		if instance.State == proto.InstPending || instance.State == proto.InstStarting || instance.State == proto.InstOptimizing {
			return "starting"
		}
	}
	for _, instance := range instances {
		// A draining or removing instance is leaving while the rest serve; it
		// does not make a running deployment read as starting. A replica
		// moved off an offline node stays removing until the node returns.
		if instance.State != proto.InstRunning && instance.State != proto.InstDraining && instance.State != proto.InstRemoving && instance.State != proto.InstFailed && instance.State != proto.InstStopped && instance.State != proto.InstPendingRemoval {
			return "starting"
		}
	}
	for _, instance := range instances {
		if instance.State == proto.InstRunning || instance.State == proto.InstDraining {
			return "running"
		}
	}
	return "starting"
}

// instanceDTO is one instance as GET /instances/{uuid} shows it, with the
// replica it fills.
func (a *apiHandlers) instanceDTO(in *store.Instance) dto.Instance {
	return a.instanceDTOs(storeLookup{a.d.Store, preparationReads{a.d.Preparation}}, []store.Instance{*in})[0]
}

// instanceDTOs are instances as the API shows them, each with the replica it
// fills; a deployment's replicas are matched once for all its instances.
func (a *apiHandlers) instanceDTOs(rows dtoLookup, insts []store.Instance) []dto.Instance {
	views := map[uint]*replicaView{}
	out := make([]dto.Instance, 0, len(insts))
	for i := range insts {
		item := a.instanceDTOFrom(rows, &insts[i])
		if dep := rows.deployment(insts[i].DeploymentID); dep != nil {
			view, seen := views[dep.ID]
			if !seen {
				if built, err := newReplicaView(rows, dep, rows.instances(dep.ID)); err == nil {
					view = &built
				}
				views[dep.ID] = view
			}
			if view != nil {
				replicaFields(&item, dep, view.match, &insts[i])
			}
		}
		out = append(out, item)
	}
	return out
}

// replicaFields fills what an instance is to its deployment's replicas, as
// the server decides it: the replica it fills and whether it takes a sleep
// or wake order of its own (planning.InstanceSleepRefusal).
func replicaFields(out *dto.Instance, dep *store.Deployment, match planning.UnitMatch, in *store.Instance) {
	if slot := match.SlotOf(in.UUID); slot != nil {
		out.Replica = &dto.InstanceReplica{Role: slot.Unit.Role, Index: slot.Unit.Index}
	}
	out.CanSleepAlone = planning.InstanceSleepRefusal(dep, match, in, true) == nil
	out.CanWakeAlone = planning.InstanceSleepRefusal(dep, match, in, false) == nil
}

func (a *apiHandlers) instanceDTOFrom(rows dtoLookup, in *store.Instance) dto.Instance {
	d := dto.Instance{
		UUID: in.UUID, DeploymentID: in.DeploymentID, State: in.State, ImageDigestID: in.ImageDigestID,
		APIPort: in.APIPort, Image: in.Image, Message: in.Message, ContainerID: in.ContainerID,
		RuntimeFailure: instanceRuntimeFailure(in),
		Phase:          in.Phase, Progress: in.Progress, BytesDone: in.BytesDone, BytesTotal: in.BytesTotal, RateBPS: in.RateBPS,
		SleepLevel: in.SleepLevel, DesiredSleep: in.SleepIntent(), Asleep: planning.InstanceAsleep(in), SleepHeld: in.SleepHeld,
		DistGroup: in.DistGroup, DistRole: instanceDistRole(in), NNodes: in.NNodes, NodeRank: in.NodeRank, Headless: in.Headless, DistAttempt: in.DistAttempt,
		GPUIndexes: parseIntSlice(in.GPUIndexes), Ports: parseIntSlice(in.Ports), Role: in.Role,
	}
	if in.Headless && in.DistGroup != "" {
		// A follower reports no level of its own; it is at its leader's,
		// the same reading the ledger uses.
		d.SleepLevel = planning.ReportedSleepLevelAmong(rows.instances(in.DeploymentID), in)
	}
	instanceNode := rows.node(in.NodeID)
	if instanceNode != nil {
		d.NodeUUID = instanceNode.UUID
	}
	if dep := rows.deployment(in.DeploymentID); dep != nil {
		if mf := rows.modelFile(dep.ModelID, in.NodeID); mf != nil {
			presence := modelNodePresence(mf, instanceNode)
			d.ModelProgress = &presence
		}
	}
	d.ModelFiles = rows.modelReads(in.ID)
	if in.ScheduleExpl != "" {
		var s any
		if json.Unmarshal([]byte(in.ScheduleExpl), &s) == nil {
			d.Schedule = s
		}
	}
	if in.DesiredStateJSON != "" {
		var spec proto.InstanceDesiredState
		if json.Unmarshal([]byte(in.DesiredStateJSON), &spec) == nil {
			d.StartTimeoutSec, d.StartTimeoutSource = spec.StartTimeout, spec.StartTimeoutSource
			d.StartTimeoutConflict, d.EngineStartTimeoutSec = spec.StartTimeoutConflict, spec.EngineStartTimeoutSeconds
			if spec.StartTimeoutSource == "" {
				d.EngineStartTimeoutSec, d.StartTimeoutConflict = deploy.ExplainRecordedStartupBudget(spec.Engine, spec.ResolvedLaunch, spec.StartTimeout)
			}
			d.StartupEstimate = dto.StartupEstimateOf(spec.StartupEstimate)
			d.PreStartArgv, d.PreStartScript = spec.PreStartArgv, spec.PreStartScript
			d.Entrypoint, d.Argv, d.Env, d.DockerArgs, d.ModelMount = spec.Entrypoint, spec.Argv, spec.Env, spec.DockerArgs, spec.ModelMount
			d.ShmBytes = spec.ShmBytes
			d.VRAMLimitBytes = spec.VRAMLimitBytes
			if d.Env == nil {
				d.Env = map[string]string{}
			}
			d.CommandMode = spec.CommandMode
			for _, mount := range spec.Mounts {
				d.Mounts = append(d.Mounts, dto.EvaluateMount{Source: mount.Source, Volume: mount.Volume, Target: mount.Target, ReadOnly: mount.ReadOnly, SizeHint: mount.SizeHint, Purpose: mount.Purpose})
			}
		}
	}
	if in.Measured != "" {
		var m any
		if json.Unmarshal([]byte(in.Measured), &m) == nil {
			d.Measured = m
		}
	}
	return d
}

func modelNodePresence(mf *store.ModelFile, node *store.Node) dto.ModelNodePresence {
	progress := 0.0
	if mf.SizeBytes > 0 {
		progress = float64(mf.Downloaded) / float64(mf.SizeBytes)
		if progress > 1 {
			progress = 1
		}
	}
	nodeUUID := ""
	if node != nil {
		nodeUUID = node.UUID
	}
	return dto.ModelNodePresence{NodeUUID: nodeUUID, NodeName: nodeDisplayName(node), State: mf.State, Path: mf.Path, Progress: progress, SizeBytes: mf.SizeBytes, Downloaded: mf.Downloaded, TransferredBytes: mf.TransferredBytes, RateBps: mf.RateBPS, RateLimitKiB: mf.RateLimit, LastRateLimitKiB: mf.LastRateLimit, Acquisition: mf.Acquisition, TransferID: mf.TransferID, TransferErrorCode: mf.TransferErrorCode, Message: mf.Message}
}

func parseIntSlice(s string) []int {
	if s == "" {
		return nil
	}
	var v []int
	_ = json.Unmarshal([]byte(s), &v)
	return v
}

// instanceDistRole is a rank's role in its multi-node generation. Rows from
// before the role was recorded read as the gateway reads them: headless or a
// rank past 0 follows; any other multi-node rank leads.
func instanceDistRole(in *store.Instance) string {
	if in.DistRole != "" || (in.NNodes <= 1 && !in.Headless && in.NodeRank == 0) {
		return in.DistRole
	}
	if in.Headless || in.NodeRank > 0 {
		return "follower"
	}
	return "leader"
}

// transferProgress is the fraction of a model file transferred so far.
func transferProgress(done, size int64) float64 {
	if size <= 0 {
		return 0
	}
	return math.Min(float64(done)/float64(size), 1)
}
