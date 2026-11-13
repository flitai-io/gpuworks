package agentupgrade

import (
	"context"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func activePhase(phase string) bool {
	return phase == "transferring" || phase == "waiting_registration" || phase == "observing_recovery"
}
func countItems(items []store.AgentUpgradeItem) Counts {
	c := Counts{Total: len(items)}
	for _, i := range items {
		switch i.Phase {
		case "queued":
			c.Queued++
		case "succeeded":
			c.Succeeded++
		case "failed":
			c.Failed++
		case "skipped":
			c.Skipped++
		case "cancelled":
			c.Cancelled++
		case "interrupted":
			c.Interrupted++
		case "unconfirmed":
			c.Unconfirmed++
		default:
			if activePhase(i.Phase) {
				c.Active++
			}
		}
	}
	c.Processed = c.Total - c.Queued - c.Active
	return c
}
func (s *Service) view(run store.AgentUpgradeRun) (RunView, error) {
	items, err := s.st.AgentUpgradeItems(run.ID)
	if err != nil {
		return RunView{}, err
	}
	v := runView(run, items)
	v.BlockedByItem, err = s.blocker()
	return v, err
}
func runView(run store.AgentUpgradeRun, items []store.AgentUpgradeItem) RunView {
	v := RunView{Record: run, Counts: countItems(items), State: "finished"}
	switch {
	case run.InterruptedReason != "":
		v.State = "interrupted"
	case v.Counts.Active > 0 && run.StopRequestedAt != nil:
		v.State = "stopping"
	case run.PauseReason != "" && (v.Counts.Queued > 0 || v.Counts.Active > 0):
		v.State = "paused"
	case v.Counts.Active > 0 || v.Counts.Queued > 0:
		v.State = "running"
	}
	return v
}
func (s *Service) Get(ctx context.Context, id uint) (RunView, error) {
	if err := ctx.Err(); err != nil {
		return RunView{}, err
	}
	run, err := s.st.AgentUpgradeRun(id)
	if err != nil {
		return RunView{}, err
	}
	return s.view(*run)
}
func pageBounds(page, size, total int) (int, int, int, int) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	start := total
	if page-1 <= total/size {
		start = min((page-1)*size, total)
	}
	end := min(start+size, total)
	return page, size, start, end
}
func (s *Service) List(ctx context.Context, page, size int) (RunPage, error) {
	runs, err := s.st.AgentUpgradeRuns()
	if err != nil {
		return RunPage{}, err
	}
	all, err := s.st.AgentUpgradeItems(0)
	if err != nil {
		return RunPage{}, err
	}
	byRun := map[uint][]store.AgentUpgradeItem{}
	for _, item := range all {
		byRun[item.RunID] = append(byRun[item.RunID], item)
	}
	page, size, start, end := pageBounds(page, size, len(runs))
	out := RunPage{Items: []RunView{}, Total: int64(len(runs)), Page: page, PageSize: size}
	out.BlockedByItem, err = s.blocker()
	if err != nil {
		return out, err
	}
	for _, r := range runs[start:end] {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		v := runView(r, byRun[r.ID])
		v.BlockedByItem = out.BlockedByItem
		out.Items = append(out.Items, v)
	}
	return out, nil
}
func (s *Service) Items(ctx context.Context, id uint, page, size int, state string) (ItemPage, error) {
	if _, err := s.st.AgentUpgradeRun(id); err != nil {
		return ItemPage{}, err
	}
	items, err := s.st.AgentUpgradeItems(id)
	if err != nil {
		return ItemPage{}, err
	}
	selected := make([]store.AgentUpgradeItem, 0, len(items))
	for _, i := range items {
		if state == "" || i.Phase == state {
			selected = append(selected, i)
		}
	}
	nodes, err := s.st.ListNodes()
	if err != nil {
		return ItemPage{}, err
	}
	versions := map[string]string{}
	for _, n := range nodes {
		versions[n.UUID] = n.AgentVersion
	}
	page, size, start, end := pageBounds(page, size, len(selected))
	out := ItemPage{Items: []ItemView{}, Total: int64(len(selected)), Page: page, PageSize: size}
	for _, i := range selected[start:end] {
		out.Items = append(out.Items, ItemView{Record: i, CurrentVersion: versions[i.NodeUUID]})
	}
	return out, ctx.Err()
}
func (s *Service) Stop(ctx context.Context, id uint, actor string) (RunView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.st.StopAgentUpgrade(id, s.now(), actor); err != nil {
		return RunView{}, err
	}
	s.notify()
	return s.Get(ctx, id)
}
func (s *Service) ContinuePreview(ctx context.Context, id uint) (Preview, error) {
	run, err := s.st.AgentUpgradeRun(id)
	if err != nil {
		return Preview{}, err
	}
	items, err := s.st.AgentUpgradeItems(id)
	if err != nil {
		return Preview{}, err
	}
	uuids := []string{}
	for _, i := range items {
		if i.Phase == "queued" || i.Phase == "failed" || i.Phase == "cancelled" || i.Phase == "interrupted" {
			uuids = append(uuids, i.NodeUUID)
		}
	}
	if len(uuids) == 0 {
		return Preview{}, &Error{Code: "AGENT_UPGRADE_INVALID_REQUEST", Detail: "there are no confirmed failed or unprocessed nodes to continue"}
	}
	p, err := s.Preview(ctx, uuids)
	if err == nil && p.BlockedByItem != nil && p.BlockedByItem.RunID == id && p.BlockedByItem.Reason == "active_run" && (run.PauseReason != "" || run.InterruptedReason != "") {
		p.BlockedByItem = nil
	}
	return p, err
}
func (s *Service) Release(ctx context.Context, id uint, uuid, actor, note string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(strings.TrimSpace(note)) < 3 || len(note) > 4096 {
		return &Error{Code: "AGENT_UPGRADE_RELEASE_NOTE_REQUIRED", Detail: "record how the old upgrade was verified to have ended"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.st.AgentUpgradeItem(id, uuid)
	if err != nil {
		return err
	}
	if item.Phase != "unconfirmed" {
		return &Error{Code: "AGENT_UPGRADE_INVALID_REQUEST", Detail: "only an unconfirmed reservation can be released"}
	}
	_, err = s.st.VerifyAgentUpgrade(item.ID, s.now(), actor, "operator_verified", strings.TrimSpace(note))
	if err == nil {
		s.notify()
	}
	return err
}
