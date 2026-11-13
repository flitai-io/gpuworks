package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// preparedAsk is one prepared ask: the endpoint's runner and the laid-out
// conversation.
type preparedAsk struct {
	runner   *aihelper.Runner
	messages []aihelper.Message
}

// seedLines bound what a deployment or node seed lists; the tools page
// through the rest.
const (
	seedInstances = 20
	seedNodeLogs  = 20
)

// prepare turns a request into a conversation: the task's system prompt, the
// seed the server gathers for its subject, the thread so far, the question.
// On a first turn the question is folded into the seed; on a follow-up the
// seed stays the task's own and the question comes after the thread.
func (a *aiService) prepare(req aihelper.AskRequest) (preparedAsk, error) {
	cfg, err := a.store.DefaultAIConfig()
	if err != nil {
		return preparedAsk{}, errNoAI
	}
	key, err := store.DecryptAIKey(a.box, cfg.APIKey)
	if err != nil {
		return preparedAsk{}, errNoAI
	}
	inSeed, after := req.Question, ""
	if len(req.History) > 0 {
		inSeed, after = "", req.Question
	}
	system, seed, err := a.seed(req.Task, req.Target, inSeed)
	if err != nil {
		return preparedAsk{}, err
	}
	client := aihelper.NewClientWithPrivateHosts(aihelper.Endpoint{BaseURL: cfg.BaseURL, APIKey: key, Model: cfg.Model}, a.privateHosts)
	prompt := aihelper.WithPlatformReading(system, time.Now())
	if req.Task == aihelper.TaskNodeHealth {
		prompt += aihelper.NodeHealthClose
	}
	return preparedAsk{
		runner:   aihelper.NewRunner(client, askEnv{aiService: a, api: req.API}),
		messages: aihelper.Conversation(aihelper.WithResponseLanguage(prompt, req.Locale, req.Task == aihelper.TaskAsk), seed, req.History, after),
	}, nil
}

// seed picks the task's system prompt and gathers its subject's context. A
// free question about a subject is seeded like that subject's task, under the
// general prompt; a free question about nothing is the question alone.
func (a *aiService) seed(task string, target aihelper.Target, question string) (system, seed string, err error) {
	switch task {
	case aihelper.TaskInstanceDiagnose:
		c, err := a.errorContext(target.InstanceUUID)
		c.Question = question
		return aihelper.SystemErrorDiagnosis, c.Prompt(), err
	case aihelper.TaskModelFit:
		c, err := a.modelContext(target.ModelID)
		c.Question = question
		return aihelper.SystemModelQA, c.Prompt(), err
	case aihelper.TaskDeploymentUnscheduled:
		c, err := a.deploymentContext(target.DeploymentID)
		c.Question = question
		return aihelper.SystemSchedulingDiagnosis, c.Prompt(), err
	case aihelper.TaskNodeHealth:
		c, err := a.nodeContext(target.NodeUUID)
		c.Question = question
		return aihelper.SystemNodeHealth, c.Prompt(), err
	}
	switch {
	case target.InstanceUUID != "":
		_, seed, err = a.seed(aihelper.TaskInstanceDiagnose, target, question)
	case target.ModelID != 0:
		_, seed, err = a.seed(aihelper.TaskModelFit, target, question)
	case target.DeploymentID != 0:
		_, seed, err = a.seed(aihelper.TaskDeploymentUnscheduled, target, question)
	case target.NodeUUID != "":
		_, seed, err = a.seed(aihelper.TaskNodeHealth, target, question)
	default:
		seed = question
	}
	return aihelper.SystemAsk, seed, err
}

// deploymentContext gathers why a deployment's replicas are not placed: what
// one replica needs, where it may go, and the scheduler's own refusal.
func (a *aiService) deploymentContext(id uint) (aihelper.DeploymentContext, error) {
	dep, err := a.store.GetDeployment(id)
	if err != nil {
		return aihelper.DeploymentContext{}, err
	}
	c := aihelper.DeploymentContext{
		DeploymentID: dep.ID, Name: dep.Name, State: string(dep.State), Engine: dep.Engine, Replicas: dep.Replicas,
		GPUNeed: planning.ReplicaGPUNeed(dep), VRAMLimitBytes: dep.VRAMLimitBytes, NNodes: dep.NNodes, Strategy: string(dep.PlacementStrategy),
	}
	if m, err := a.store.GetModel(dep.ModelID); err == nil {
		c.Model = m.Name
	}
	if reqs, err := a.store.DeploymentRequirements(dep.ID); err == nil {
		c.Requirements = reqs
	}
	insts, err := a.store.InstancesForDeployment(dep.ID)
	if err != nil {
		return aihelper.DeploymentContext{}, err
	}
	names := map[uint]string{}
	for i := range insts {
		if i == seedInstances {
			break
		}
		in := insts[i]
		if _, ok := names[in.NodeID]; !ok {
			if n, err := a.store.NodeByID(in.NodeID); err == nil {
				names[in.NodeID] = n.DisplayName()
			}
		}
		c.Instances = append(c.Instances, aihelper.InstanceLine{UUID: in.UUID, State: in.State, Node: names[in.NodeID], Message: in.Message})
	}
	if ev, found, err := a.store.LatestScheduleEvent(dep.ID); err == nil && found {
		var data any
		_ = json.Unmarshal([]byte(ev.Data), &data)
		c.Refusal = map[string]any{"code": ev.Code, "message": ev.Message, "data": data}
		c.RefusalAtMS = ev.CreatedAt.UnixMilli()
		if first, found, err := a.store.FirstScheduleEvent(dep.ID); err == nil && found {
			c.FirstRefusalMS = first.CreatedAt.UnixMilli()
		}
	}
	return c, nil
}

// nodeContext gathers a node health check: each card's health level and the
// agent's evidence, the node's state, what runs there, and its recent
// warnings from the node and agent logs.
func (a *aiService) nodeContext(uuid string) (aihelper.NodeContext, error) {
	n, err := a.store.NodeWithGPUs(uuid)
	if err != nil {
		return aihelper.NodeContext{}, err
	}
	c := aihelper.NodeContext{
		NodeUUID: n.UUID, Name: n.DisplayName(), State: n.ConnectionState(), Scheduling: string(n.Scheduling),
		RuntimeGPUBlocked: n.RuntimeGPUBlocked, GPUProbeError: n.GPUProbeError, LogLimit: 2 * seedNodeLogs,
		RuntimeDriver: n.RuntimeDriver, RuntimeFallbackFrom: n.RuntimeFallbackFrom, RuntimeFallbackReason: n.RuntimeFallbackReason,
	}
	if c.Scheduling == "" {
		c.Scheduling = "schedulable"
	}
	if n.LastSeenAt != nil {
		c.LastSeenMS = n.LastSeenAt.UnixMilli()
	}
	if hw, err := a.NodeProfile(uuid); err == nil {
		c.Hardware = hw
	}
	// Every card, not only the node's own: "why can this card not serve"
	// is the question, and lent to a virtual machine is an answer.
	for _, g := range n.GPUs {
		var foreign []string
		for _, p := range g.ForeignUse() {
			foreign = append(foreign, fmt.Sprintf("%s pid %d", p.Name, p.PID))
		}
		c.GPUs = append(c.GPUs, aihelper.GPUHealthLine{
			Index: g.Index, PCIeBDF: g.PCIeBDF, Name: g.Name, Level: gpuHealthLevel(g), Code: g.HealthCode,
			Schedulable: g.CanTakeWork(), Ownership: g.Ownership, ForeignUse: foreign, Evidence: g.HealthMessage,
		})
	}
	if t := a.store.NodeTopology(n.ID); t != nil {
		c.FastCliques = t.FastCliques
	}
	insts, err := a.store.InstancesForNode(n.ID)
	if err != nil {
		return aihelper.NodeContext{}, err
	}
	for i, in := range insts {
		if i == seedInstances {
			break
		}
		c.Instances = append(c.Instances, aihelper.InstanceLine{UUID: in.UUID, State: in.State, Message: in.Message})
	}
	if a.obs != nil {
		var lines []aihelper.LogLine
		for _, source := range []string{logstore.SourceNode, logstore.SourceAgent} {
			res, err := a.obs.logs.Search(logstore.Filter{Node: uuid, Source: source, Level: logstore.LevelWarn, Limit: seedNodeLogs})
			if err != nil {
				return aihelper.NodeContext{}, err
			}
			for _, e := range res.Entries {
				lines = append(lines, aihelper.LogLine{TimeMS: e.TimeMS, Level: e.Level, Line: "[" + source + "] " + e.Line})
			}
		}
		sortLogLines(lines)
		c.Logs = lines
	}
	return c, nil
}

// gpuHealthLevel is the card's level, lower-cased for the assistant's
// vocabulary. The level itself is normalized when the card is stored
// (accelerator.CardHealthLevel), so there is nothing to interpret here.
func gpuHealthLevel(g store.GPU) string {
	return strings.ToLower(accelerator.NormalizeHealthLevel(g.HealthStatus))
}

func sortLogLines(lines []aihelper.LogLine) {
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].TimeMS < lines[j].TimeMS })
}
