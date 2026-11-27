package dto

import "time"

// SchedulingTimeline is the scheduling center's residency timeline for some
// nodes: for each GPU, which deployment's instance held it and in what state
// over a window (a quick one ending now, or a custom range), and the stats of
// the deployments that share cards.
type SchedulingTimeline struct {
	// Scope says what the deployments' figures count: TimelineScopeNode,
	// only their instances on the named nodes; TimelineScopeFleet, all of
	// them. The UI says so from this and does not work it out.
	Scope  string         `json:"scope"`
	Window TimelineWindow `json:"window"`
	// Windows are the windows the request may ask for, the default first:
	// the UI offers these and no list of its own.
	Windows     []TimelineWindowOption `json:"windows"`
	Nodes       []TimelineNode         `json:"nodes"`
	Deployments []TimelineDeployment   `json:"deployments"`
	// SwitchesAvailable says the deployments' counts of yields, yields to
	// them and wakes (Yielded, YieldedTo, Wakes, Switches) are known for the
	// window. They come from the scheduling activity's memory, which holds
	// the last 24 hours, so a node's range that begins earlier has none: the
	// four are then 0 and mean "not known", and the UI shows no figure.
	SwitchesAvailable bool `json:"switches_available"`
}

// The scopes of a timeline's deployment figures.
const (
	TimelineScopeNode  = "node"
	TimelineScopeFleet = "fleet"
)

// TimelineWindow is the stretch of time the timeline covers: a quick window
// ends at To, which is now, and Key names it (one of Windows); a custom range
// (Custom true, Key TimelineWindowCustom) is the one asked for with since and
// until. Seconds is its length and Hours its whole hours (0 for a range under
// an hour).
type TimelineWindow struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Hours   int       `json:"hours"`
	Key     string    `json:"key"`
	Seconds int64     `json:"seconds"`
	Custom  bool      `json:"custom"`
}

// TimelineWindowCustom is the Key of a custom range, which is no entry of
// the Windows a read lists.
const TimelineWindowCustom = "custom"

// TimelineWindowOption is one window a timeline may be asked for.
type TimelineWindowOption struct {
	Key     string `json:"key"`
	Seconds int64  `json:"seconds"`
}

// TimelineNode is one node and its GPUs' timelines.
type TimelineNode struct {
	UUID string        `json:"uuid"`
	Name string        `json:"name"`
	GPUs []TimelineGPU `json:"gpus"`
}

// TimelineGPU is one GPU's segments, oldest first. A card that changed hands
// so often that its segments would not fit the answer lists the newest ones
// raw (Segments) and the time before them as at most 50 summaries (Summary);
// SummarizedBefore is where the two meet: Summary covers [window start,
// SummarizedBefore) and Segments [SummarizedBefore, window end], and both are
// absent when nothing was summarized. A segment that straddles
// SummarizedBefore is split there. Truncated says some of the card's history
// is not listed raw (it is true whenever SummarizedBefore is set); a client
// that knows no Summary draws only Segments, which is what it did before
// summaries existed.
type TimelineGPU struct {
	Index            int               `json:"index"`
	Segments         []TimelineSegment `json:"segments"`
	Truncated        bool              `json:"truncated"`
	SummarizedBefore *time.Time        `json:"summarized_before,omitempty"`
	Summary          []TimelineSummary `json:"summary,omitempty"`
}

// TimelineSummaryKind is the Kind of a TimelineSummary.
const TimelineSummaryKind = "summary"

// TimelineSummary is one stretch of the time a card's raw segments no longer
// list: the equal slice [From, To) of it, and what the card did in it. Raw
// segments carry no kind; a summary says so with Kind.
//
// Switches counts the times the card changed hands in the slice: the
// deployment awake on it became another one (a deployment going to sleep and
// waking again is none), counted in the slice its new holder began in. A
// slice is listed only when something held the card in it.
//
// Deployments are those that held the card in the slice, with the seconds
// each spent in each state (an instance's segments of one state counted once
// where they overlap). The seconds of different deployments overlap when
// they share a card at the same time and may fall short of the slice where
// nothing held the card, and each is rounded, so they do not add up to the
// slice's length. DominantDeploymentID and Dominant name the deployment awake
// longest in the slice (the lower id on a tie), absent when none was awake;
// they are kept for clients of the first summary shape, the UI draws each
// deployment's own State instead.
type TimelineSummary struct {
	Kind                 string                      `json:"kind"`
	From                 time.Time                   `json:"from"`
	To                   time.Time                   `json:"to"`
	Switches             int                         `json:"switches"`
	DominantDeploymentID uint                        `json:"dominant_deployment_id,omitempty"`
	Dominant             string                      `json:"dominant_deployment,omitempty"`
	Deployments          []TimelineSummaryDeployment `json:"deployments"`
}

// TimelineSummaryDeployment is what one deployment's instances did with a
// card during a TimelineSummary, in seconds: awake (serving), asleep (any
// level), switching (between a sleep or wake order and its result) and
// starting.
//
// State is the one of them the deployment spent the most time in during the
// slice, in the words of TimelineSegment.State (awake, asleep, switching,
// starting); on a tie of the unrounded times the first of awake, switching,
// starting, asleep wins. Level is, as on a segment, the sleep level (1 or 2)
// and only when State is asleep: the one with most asleep time in the slice
// (the shallower on a tie), absent when no asleep stretch names a level. The
// server decides, the UI only draws the slice in the colour of State.
type TimelineSummaryDeployment struct {
	DeploymentID      uint   `json:"deployment_id"`
	Deployment        string `json:"deployment"`
	DeploymentDeleted bool   `json:"deployment_deleted,omitempty"`
	AwakeSeconds      int64  `json:"awake_seconds"`
	AsleepSeconds     int64  `json:"asleep_seconds"`
	SwitchingSeconds  int64  `json:"switching_seconds"`
	StartingSeconds   int64  `json:"starting_seconds"`
	State             string `json:"state"`
	Level             int    `json:"level,omitempty"`
}

// TimelineSegment is one stretch of an instance holding a GPU in one state:
// awake, asleep (Level says how deep), switching (between a sleep or wake
// order and its result) or starting. From and To are inside the window.
type TimelineSegment struct {
	DeploymentID      uint      `json:"deployment_id"`
	Deployment        string    `json:"deployment"`
	DeploymentDeleted bool      `json:"deployment_deleted,omitempty"`
	Instance          string    `json:"instance"`
	State             string    `json:"state"`
	Level             int       `json:"level,omitempty"`
	From              time.Time `json:"from"`
	To                time.Time `json:"to"`
}

// TimelineDeployment is what a deployment that shares GPUs (any yield policy
// but never, or a deleted one that gave its cards up or was given another's
// in the window) did over the window, counted as the timeline's Scope says.
// DeploymentDeleted marks a deleted one: Deployment is the name it left
// behind and YieldPolicy is empty (not kept).
// Over the fleet, PresentSeconds is the part of the window it existed in:
// from its creation (or its first instance, when earlier or the creation is
// not kept) or the window's start, to its deletion or the window's end; on
// nodes, the part of the window at least one of its instances there held
// cards. AwakeSeconds is the time at least one of its instances (there) was
// awake and AwakeShare that over PresentSeconds (0 to 1; 0 when
// PresentSeconds is). Yielded counts the times it was put to sleep for
// another deployment, YieldedTo those another (existing or since deleted)
// was put to sleep for it, Wakes those the platform woke it - on nodes, only
// the events whose cards are there - and Switches is Yielded plus Wakes.
// HeldRequests counts the requests the gateway held while it woke, the
// deployment's in either scope (the gateway holds a request for the
// deployment, not an instance); the waits are approximated from the
// gateway's per-interval quantiles weighted by the requests of each interval
// and are absent when no request was held or the gateway's metrics are not
// kept.
type TimelineDeployment struct {
	DeploymentID      uint   `json:"deployment_id"`
	Deployment        string `json:"deployment"`
	DeploymentDeleted bool   `json:"deployment_deleted,omitempty"`
	// YieldPolicy is its yield policy (never, idle, timeslice, anytime);
	// empty for a deleted deployment.
	YieldPolicy    string   `json:"yield_policy"`
	AwakeSeconds   int64    `json:"awake_seconds"`
	AwakeShare     float64  `json:"awake_share"`
	PresentSeconds int64    `json:"present_seconds"`
	Switches       int      `json:"switches"`
	Yielded        int      `json:"yielded"`
	YieldedTo      int      `json:"yielded_to"`
	Wakes          int      `json:"wakes"`
	HeldRequests   int      `json:"held_requests"`
	WaitP50Ms      *float64 `json:"wait_p50_ms,omitempty"`
	WaitP95Ms      *float64 `json:"wait_p95_ms,omitempty"`
	// Cards is where its live instances are now, by node; empty when none
	// is placed.
	Cards []TimelineCards `json:"cards"`
}

// TimelineCards is the GPUs a deployment's instances hold on one node now;
// Node is empty for a node no longer known, and so is NodeUUID, by which
// the UI opens the node.
type TimelineCards struct {
	NodeID   uint   `json:"node_id"`
	NodeUUID string `json:"node_uuid,omitempty"`
	Node     string `json:"node"`
	GPUs     []int  `json:"gpus"`
}
