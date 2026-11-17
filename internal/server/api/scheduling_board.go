package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

const (
	// boardPendingWindow is how recent a placement refusal must be for the
	// board to list its deployment as waiting.
	boardPendingWindow = 30 * time.Minute
	// boardUpcomingWindow is how far ahead the board lists schedule actions.
	boardUpcomingWindow = 24 * time.Hour
)

// schedulingBoard answers GET /scheduling/board: a page of nodes (page,
// page_size, q as for GET /nodes) with each GPU's holders, and fleet-wide what
// waits and what is scheduled next. With node=<uuid> it
// answers for that one node alone, without the fleet-wide lists (the drawer of
// the overview reads it). With queues=1 it answers the fleet-wide lists alone
// and reads no node. It only reads.
func (a *apiHandlers) schedulingBoard(w http.ResponseWriter, r *http.Request) {
	if uuid := strings.TrimSpace(r.URL.Query().Get("node")); uuid != "" {
		a.schedulingBoardNode(w, uuid)
		return
	}
	query, err := parseListQuery(r, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	nameOf, levelOf := a.deploymentLabels()
	board := dto.SchedulingBoard{
		Nodes:   dto.ListPage[dto.BoardNode]{Items: []dto.BoardNode{}, Page: query.page, PageSize: query.pageSize},
		Pending: []dto.BoardPending{}, WaitingWakes: []dto.BoardWaitingWake{}, Upcoming: []dto.BoardUpcomingAction{},
	}
	// queues=1 asks for the fleet-wide lists alone, which is what the
	// overview page reads beside its heatmap: no node is read for it.
	if r.URL.Query().Get("queues") != "1" {
		nodes, err := a.d.Store.FleetNodes()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		var matched []store.Node
		for _, node := range nodes {
			if query.matchesSearch(node.Name, node.Hostname, node.UUID) {
				matched = append(matched, node)
			}
		}
		sort.SliceStable(matched, func(i, j int) bool { return matched[i].DisplayName() < matched[j].DisplayName() })
		online := a.onlineOf(nodes)
		board.Nodes = pageOf(matched, query, func(rows []store.Node) []dto.BoardNode {
			out := make([]dto.BoardNode, 0, len(rows))
			for i := range rows {
				out = append(out, a.boardNode(&rows[i], online(rows[i].UUID), nameOf, levelOf))
			}
			return out
		})
	}
	now := time.Now().UTC()
	if board.Pending, err = a.boardPending(now, nameOf); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if board.WaitingWakes, err = a.boardWaitingWakes(nameOf); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if board.Upcoming, err = a.boardUpcoming(now, nameOf); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, board)
}

// deploymentLabels name a deployment and give its yield policy, each
// deployment read once however often it is asked for.
func (a *apiHandlers) deploymentLabels() (nameOf func(uint) string, levelOf func(uint) string) {
	type labelled struct{ name, level string }
	deployments := map[uint]labelled{}
	lookup := func(id uint) labelled {
		if known, ok := deployments[id]; ok {
			return known
		}
		known := labelled{level: yieldpolicy.Never}
		if dep, err := a.d.Store.GetDeployment(id); err == nil {
			known = labelled{name: dep.Name, level: dep.Policy().Yield}
		}
		deployments[id] = known
		return known
	}
	return func(id uint) string { return lookup(id).name }, func(id uint) string { return lookup(id).level }
}

func (a *apiHandlers) boardNode(node *store.Node, online bool, nameOf func(uint) string, levelOf func(uint) string) dto.BoardNode {
	out := dto.BoardNode{UUID: node.UUID, Name: nodeDisplayName(node), Online: online, GPUs: []dto.BoardGPU{}}
	withGPUs, err := a.d.Store.NodeWithGPUs(node.UUID)
	if err != nil {
		return out
	}
	// The cards are the overview's (planning.NodeCards gives one node what
	// planning.FleetCards gives the fleet), state included.
	cards, err := planning.NodeCards(a.d.Store, withGPUs)
	if err != nil {
		return out
	}
	for _, card := range cards {
		item := dto.BoardGPU{
			Index: card.Index, Name: card.Name, MemoryBytes: card.MemoryBytes, AwakeBytes: card.AwakeBytes, SleepingBytes: card.SleepingBytes,
			FreeBytes: max(card.MemoryBytes-card.AwakeBytes-card.SleepingBytes, 0), State: string(card.State(online)),
			HeldOutside: card.HeldOutside, Switching: card.Switching, Occupants: []dto.BoardOccupant{},
		}
		for _, o := range card.Occupants {
			state := "awake"
			if o.Asleep {
				state = "asleep"
			}
			item.Occupants = append(item.Occupants, dto.BoardOccupant{
				DeploymentID: o.DeploymentID, Deployment: nameOf(o.DeploymentID), YieldPolicy: levelOf(o.DeploymentID), Instance: o.Instance,
				State: state, SleepLevel: o.SleepLevel, ClaimBytes: o.ClaimBytes, ResidueBytes: o.ResidueBytes,
			})
		}
		out.GPUs = append(out.GPUs, item)
	}
	return out
}

// boardPending is every active deployment whose latest scheduling note is a
// recent refusal to place it.
func (a *apiHandlers) boardPending(now time.Time, nameOf func(uint) string) ([]dto.BoardPending, error) {
	latest, err := a.d.Store.RecentLatestScheduleEvents(now.Add(-boardPendingWindow))
	if err != nil {
		return nil, err
	}
	out := []dto.BoardPending{}
	for depID, ev := range latest {
		if ev.Code != store.EventDeploymentUnschedulable || now.Sub(ev.CreatedAt) > boardPendingWindow {
			continue
		}
		dep, err := a.d.Store.GetDeployment(depID)
		if err != nil || dep.State != store.DeployActive {
			continue
		}
		// A refusal the deployment has since got past (its replicas are all
		// placed now, say after the agents reconnected) is history, not a
		// wait: only a deployment with a replica no instance fills waits.
		if waits, err := a.hasUnfilledReplica(dep); err != nil || !waits {
			continue
		}
		item := dto.BoardPending{DeploymentID: depID, Deployment: nameOf(depID), At: ev.CreatedAt.UTC()}
		var data struct {
			Scheduler *dto.ScheduleReason `json:"scheduler"`
		}
		if json.Unmarshal([]byte(ev.Data), &data) == nil && data.Scheduler != nil {
			item.Reason = *data.Scheduler
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// hasUnfilledReplica says some replica (or unit) of the deployment has no
// instance filling it: what still waits to be placed.
func (a *apiHandlers) hasUnfilledReplica(dep *store.Deployment) (bool, error) {
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		return false, err
	}
	match, err := planning.DeploymentMatch(a.d.Store, dep, insts)
	if err != nil {
		return false, err
	}
	return match.Unfilled(), nil
}

func (a *apiHandlers) boardWaitingWakes(nameOf func(uint) string) ([]dto.BoardWaitingWake, error) {
	insts, err := a.d.Store.InstancesWakeWaiting()
	if err != nil {
		return nil, err
	}
	out := []dto.BoardWaitingWake{}
	at := map[uint]int{}
	for _, in := range insts {
		index, ok := at[in.DeploymentID]
		if !ok {
			at[in.DeploymentID] = len(out)
			out = append(out, dto.BoardWaitingWake{DeploymentID: in.DeploymentID, Deployment: nameOf(in.DeploymentID), Since: in.WakeWaitingSince.UTC(), Instances: []string{}})
			index = len(out) - 1
		}
		out[index].Instances = append(out[index].Instances, in.UUID)
	}
	return out, nil
}

func (a *apiHandlers) boardUpcoming(now time.Time, nameOf func(uint) string) ([]dto.BoardUpcomingAction, error) {
	rows, err := a.d.Store.EnabledLifecycleSchedules()
	if err != nil {
		return nil, err
	}
	out := []dto.BoardUpcomingAction{}
	for _, row := range rows {
		for _, at := range automationSchedule(row).Occurrences(now, now.Add(boardUpcomingWindow)) {
			item := dto.BoardUpcomingAction{
				DeploymentID: row.DeploymentID, Deployment: nameOf(row.DeploymentID), ScheduleID: row.ID, Action: row.Action,
				SleepLevel: row.SleepLevel, At: at.UTC(),
			}
			if row.Action == store.ScheduleActionReplicas {
				replicas := row.Replicas
				item.Replicas, item.Role = &replicas, row.Role
			}
			out = append(out, item)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}
