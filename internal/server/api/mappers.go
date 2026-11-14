package api

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
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
	return a.deploymentDTOFrom(deployment.NewStoreRows(a.d.Store, a.d.Preparation), d, withInstances)
}

// deploymentDTOFrom builds the DTO from whichever lookup the caller holds: the
// store for one deployment, a preloaded set for a list.
func (a *apiHandlers) deploymentDTOFrom(rows deployment.Rows, d *store.Deployment, withInstances bool) dto.Deployment {
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
	insts := rows.Instances(d.ID)
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
	var facts deployment.Facts
	if viewErr == nil {
		facts = deployment.FactsOf(rows, view.ReplicaView, insts)
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
	model := rows.Model(d.ModelID)
	contract := inferenceapi.ResolveDeploymentContract(nil, d) // no operations
	if model != nil && d.ModelID != 0 {
		contract = inferenceapi.ModelContract(model)
	}
	out.HealthCheckable = d.ModelID != 0 && deployment.HealthCheckable(model)
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
		if n := rows.Node(id); n != nil {
			out.NodeUUIDs = append(out.NodeUUIDs, n.UUID)
			out.NodeNames = append(out.NodeNames, nodeDisplayName(n))
		}
	}
	if model != nil {
		out.ModelName = model.Name
	}
	out.NodeRequirements = rows.Requirements(d.ID)
	if out.NodeRequirements == nil {
		out.NodeRequirements = map[string][]string{}
	}
	out.PlacementStrategy = string(d.PlacementStrategy)
	primaryNode := rows.Node(d.NodeID)
	if primaryNode != nil {
		out.NodeUUID = primaryNode.UUID
	}
	progress, recorded := deployment.ModelProgress(rows, d)
	out.ModelProgress = progress
	if primaryNode != nil {
		out.NodeName = nodeDisplayName(primaryNode)
	}
	out.RuntimeNodes, out.RuntimeNodeNames = deploymentRuntimeNodes(rows, insts)
	out.RuntimeState = deployment.RuntimeState(rows, d, insts, recorded)
	out.SleepLevel = facts.Level
	out.AsleepReplicas = facts.AsleepReplicas
	out.SleepHold = facts.SleepHold()
	out.SleepHeld = out.SleepHold == dto.SleepHoldAll
	out.SleepWakesOnRequest = planning.OperatorSleepWakesOnRequest(d)
	out.State = deployment.EffectiveState(d, out.RuntimeState, facts)
	if withInstances {
		out.NodeRequirementLabels = a.requirementLabels(out.NodeRequirements)
		for i := range insts {
			item := a.instanceDTOFrom(rows, &insts[i])
			if viewErr == nil {
				replicaFields(&item, d, view.Match, &insts[i])
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
func deploymentRuntimeNodes(rows deployment.Rows, insts []store.Instance) ([]dto.DeploymentRuntimeNode, []string) {
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
		node := rows.Node(nodeID)
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

// deploymentListStates are the states a deployment list can be filtered by.
var deploymentListStates = deployment.States

// instanceDTO is one instance as GET /instances/{uuid} shows it, with the
// replica it fills.
func (a *apiHandlers) instanceDTO(in *store.Instance) dto.Instance {
	return a.instanceDTOs(deployment.NewStoreRows(a.d.Store, a.d.Preparation), []store.Instance{*in})[0]
}

// instanceDTOs are instances as the API shows them, each with the replica it
// fills; a deployment's replicas are matched once for all its instances.
func (a *apiHandlers) instanceDTOs(rows deployment.Rows, insts []store.Instance) []dto.Instance {
	views := map[uint]*replicaView{}
	out := make([]dto.Instance, 0, len(insts))
	for i := range insts {
		item := a.instanceDTOFrom(rows, &insts[i])
		if dep := rows.Deployment(insts[i].DeploymentID); dep != nil {
			view, seen := views[dep.ID]
			if !seen {
				if built, err := newReplicaView(rows, dep, rows.Instances(dep.ID)); err == nil {
					view = &built
				}
				views[dep.ID] = view
			}
			if view != nil {
				replicaFields(&item, dep, view.Match, &insts[i])
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

func (a *apiHandlers) instanceDTOFrom(rows deployment.Rows, in *store.Instance) dto.Instance {
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
		d.SleepLevel = planning.ReportedSleepLevelAmong(rows.Instances(in.DeploymentID), in)
	}
	instanceNode := rows.Node(in.NodeID)
	if instanceNode != nil {
		d.NodeUUID = instanceNode.UUID
	}
	if dep := rows.Deployment(in.DeploymentID); dep != nil {
		if mf := rows.ModelFile(dep.ModelID, in.NodeID); mf != nil {
			presence := deployment.ModelNodePresence(mf, instanceNode)
			d.ModelProgress = &presence
		}
	}
	d.ModelFiles = rows.ModelReads(in.ID)
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
