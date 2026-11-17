package dto

// DataRetention is how long each kind of observation and statistic is kept,
// and the time zone statistics count days in (store.DataRetention).
type DataRetention struct {
	MetricsHours    int    `json:"metrics_hours"`
	LogsHours       int    `json:"logs_hours"`
	RequestLogDays  int    `json:"request_log_days"`
	RequestLogMaxMB int    `json:"request_log_max_mb"`
	UsageHourlyDays int    `json:"usage_hourly_days"`
	UsageDailyDays  int    `json:"usage_daily_days"`
	StatsHourlyDays int    `json:"stats_hourly_days"`
	StatsDailyDays  int    `json:"stats_daily_days"`
	Quality         bool   `json:"quality"`
	GPU             bool   `json:"gpu"`
	GPUTime         bool   `json:"gpu_time"`
	TimeZone        string `json:"time_zone"`
}

// DataRetentionLimit is the range a numeric value may take.
type DataRetentionLimit struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// DataRetentionView is the setting with what the settings page needs to edit
// it: every numeric value's bounds, by JSON name, and the server's own zone.
type DataRetentionView struct {
	Value          DataRetention                 `json:"value"`
	Limits         map[string]DataRetentionLimit `json:"limits"`
	ServerTimeZone string                        `json:"server_time_zone"`
}

// DataRetentionStore is what one kind of data holds now.
type DataRetentionStore struct {
	// Kind is metrics, logs, requests, usage, quality, gpu or gpu_time.
	Kind string `json:"kind"`
	// File is the database file the kind lives in (metrics.db, logs.db,
	// ux.db, stats.db, or the main database for usage).
	File string `json:"file"`
	// FileBytes is that file's size on disk; null for the main database,
	// which holds far more than the usage. Kinds sharing stats.db report the
	// same size.
	FileBytes *int64 `json:"file_bytes"`
	// HourlyRows and DailyRows count a statistic's rows; null for the raw
	// observations.
	HourlyRows *int64 `json:"hourly_rows"`
	DailyRows  *int64 `json:"daily_rows"`
	// EarliestMS is the oldest time the kind can still answer for (Unix ms):
	// what the request log holds, the retention edge of raw samples and
	// lines; null when there is nothing.
	EarliestMS *int64 `json:"earliest_ms"`
}

// DataRetentionPass is the last run of a background pass; zero AtMS when it
// has not run.
type DataRetentionPass struct {
	AtMS  int64  `json:"at_ms"`
	Error string `json:"error,omitempty"`
}

// DataRetentionStatus is the retention's effect: the stores, when live
// accumulation of the statistics began, how far the one-off fill from the
// request log got, and the background passes.
type DataRetentionStatus struct {
	Stores      []DataRetentionStore `json:"stores"`
	LiveSinceMS int64                `json:"live_since_ms"`
	Backfill    StatisticsBackfill   `json:"backfill"`
	Retain      DataRetentionPass    `json:"retain"`
	Rollup      DataRetentionPass    `json:"rollup"`
	// UsageFlush is the last token usage write: its rows and how long it
	// took, the cost that grows with the key x deployment combinations used.
	UsageFlush UsageFlush `json:"usage_flush"`
}

// StatisticsBackfill is how far back the fill reaches (Unix ms) per store.
type StatisticsBackfill struct {
	UsageDone        bool  `json:"usage_done"`
	UsageThroughMS   int64 `json:"usage_through_ms"`
	QualityDone      bool  `json:"quality_done"`
	QualityThroughMS int64 `json:"quality_through_ms"`
}

// UsageFlush describes the last token usage write.
type UsageFlush struct {
	AtMS  int64  `json:"at_ms"`
	Rows  int    `json:"rows"`
	MS    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// AnalyticsRange is the span a statistics answer covers, as the server
// resolved it: day spans are whole days of the statistics time zone.
type AnalyticsRange struct {
	FromMS      int64  `json:"from_ms"`
	ToMS        int64  `json:"to_ms"`
	Granularity string `json:"granularity"` // hour | day
	TimeZone    string `json:"time_zone"`
	// EarliestMS is the oldest bucket the statistic still has at this
	// granularity; 0 when it has none.
	EarliestMS int64 `json:"earliest_ms"`
	// LiveSinceMS is when live accumulation began: buckets before it were
	// filled in once from the request log (sampled rows weighed, requests
	// whose usage the gateway did not read then left without tokens).
	LiveSinceMS int64 `json:"live_since_ms"`
	// Buckets are the bucket starts, in order; series align with them.
	Buckets []int64 `json:"buckets"`
}

// UsageCounts are a usage total (rollup.Usage): tokens add up the requests
// that reported usage; the hit rate divides cached_tokens by
// cache_reported_prompt.
type UsageCounts struct {
	Requests            int64 `json:"requests"`
	NoUsage             int64 `json:"no_usage"`
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	ReasoningReported   int64 `json:"reasoning_reported"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReported       int64 `json:"cache_reported"`
	CacheReportedPrompt int64 `json:"cache_reported_prompt"`
}

// UsageRow is one key x user x deployment x source over the range.
type UsageRow struct {
	APIKeyID      uint   `json:"api_key_id"`
	APIKeyName    string `json:"api_key_name"`
	APIKeyPrefix  string `json:"api_key_prefix"`
	APIKeyRevoked bool   `json:"api_key_revoked"`
	// APIKeyDeleted marks a key deleted before keys were kept on revoking.
	APIKeyDeleted     bool   `json:"api_key_deleted"`
	UserID            uint   `json:"user_id"`
	UserName          string `json:"user_name"`
	DeploymentID      uint   `json:"deployment_id"`
	DeploymentName    string `json:"deployment_name"`
	ServedName        string `json:"served_name"`
	DeploymentDeleted bool   `json:"deployment_deleted"`
	Source            string `json:"source"` // api | playground
	// FirstMS and LastMS are the first and last buckets it was used in.
	FirstMS int64 `json:"first_ms"`
	LastMS  int64 `json:"last_ms"`
	UsageCounts
}

// UsageAnalytics is the token usage over a range.
type UsageAnalytics struct {
	Range  AnalyticsRange `json:"range"`
	Totals UsageCounts    `json:"totals"`
	// Series is the total of each bucket.
	Series []UsageCounts `json:"series"`
	Rows   []UsageRow    `json:"rows"`
}

// QualityFigures are a quality total: outcomes, latency percentiles of the
// whole replies (null without any) and the output speed of the streamed ones.
type QualityFigures struct {
	Requests     int64    `json:"requests"`
	Completed    int64    `json:"completed"`
	Length       int64    `json:"length"`
	ClientClosed int64    `json:"client_closed"`
	Interrupted  int64    `json:"interrupted"`
	Error        int64    `json:"error"`
	TTFTP50MS    *float64 `json:"ttft_p50_ms"`
	TTFTP95MS    *float64 `json:"ttft_p95_ms"`
	E2EP50MS     *float64 `json:"e2e_p50_ms"`
	E2EP95MS     *float64 `json:"e2e_p95_ms"`
	// OutputTokensPerSec is completion tokens over the time from the first
	// token to the last event; null without a timed stream.
	OutputTokensPerSec *float64 `json:"output_tokens_per_sec"`
}

// QualityRow is a deployment's quality over the range.
type QualityRow struct {
	DeploymentID      uint   `json:"deployment_id"`
	DeploymentName    string `json:"deployment_name"`
	ServedName        string `json:"served_name"`
	DeploymentDeleted bool   `json:"deployment_deleted"`
	QualityFigures
}

// QualityAnalytics is the service quality over a range.
type QualityAnalytics struct {
	Range  AnalyticsRange   `json:"range"`
	Totals QualityFigures   `json:"totals"`
	Series []QualityFigures `json:"series"`
	Rows   []QualityRow     `json:"rows"`
	// Classes splits the totals by the priority class the gateway ran the
	// requests at, most requests first; the class "" holds the requests
	// recorded before classes existed (whose class is unknown, not none).
	Classes []QualityClassRow `json:"classes"`
}

// QualityClassRow is the quality of the requests run at one priority class.
type QualityClassRow struct {
	PriorityClass string `json:"priority_class"`
	QualityFigures
}

// GPUFigures are a card's or a group's statistics: means over the samples,
// maxima over them; null without samples of that kind.
type GPUFigures struct {
	UtilAvg      *float64 `json:"util_avg"`
	UtilMax      *float64 `json:"util_max"`
	MemUsedAvg   *float64 `json:"mem_used_avg_bytes"`
	MemUsedMax   *float64 `json:"mem_used_max_bytes"`
	MemTotal     *float64 `json:"mem_total_bytes"`
	PowerAvg     *float64 `json:"power_avg_watts"`
	TempAvg      *float64 `json:"temp_avg_celsius"`
	TempMax      *float64 `json:"temp_max_celsius"`
	UtilSamples  int64    `json:"util_samples"`
	PowerSamples int64    `json:"power_samples"`
}

// GPUCardRow is one card over the range.
type GPUCardRow struct {
	NodeUUID string `json:"node_uuid"`
	NodeName string `json:"node_name"`
	// NodeDeleted marks a card of a deleted node, named by the name the node
	// left.
	NodeDeleted bool   `json:"node_deleted"`
	GPUIndex    int    `json:"gpu_index"`
	GPUUUID     string `json:"gpu_uuid"`
	GPUModel    string `json:"gpu_model"`
	// Deployments hold the card now (empty when none, or the card is gone).
	Deployments []string `json:"deployments"`
	// Present says the card is in the inventory now, whether its node is
	// online or not.
	Present bool `json:"present"`
	GPUFigures
}

// GPUHeatRow is one row of the heat map: a model, a node or a card.
type GPUHeatRow struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// NodeDeleted marks the row of a deleted node, or of one of its cards.
	NodeDeleted bool `json:"node_deleted"`
	// Values align with the range's buckets; null where the row had no
	// samples of the metric.
	Values []*float64 `json:"values"`
}

// GPUGroupRow is a model's or a node's cards over the range.
type GPUGroupRow struct {
	Key   string `json:"key"` // the model, or the node's UUID
	Label string `json:"label"`
	// NodeDeleted marks a deleted node's row; its cards are no longer in the
	// inventory, so Cards and Nodes count none.
	NodeDeleted bool `json:"node_deleted"`
	// Cards and Nodes count the group's cards and their nodes in the
	// inventory now; Models are the models of a node's cards.
	Cards  int      `json:"cards"`
	Nodes  int      `json:"nodes"`
	Models []string `json:"models"`
	// Deployments hold the group's cards now, each with how many;
	// Unassigned counts the cards no deployment holds.
	Deployments []GPUHolding `json:"deployments"`
	Unassigned  int          `json:"unassigned"`
	GPUFigures
}

// GPUHolding is a deployment holding cards of a group.
type GPUHolding struct {
	Name  string `json:"name"`
	Cards int    `json:"cards"`
}

// GPUModelCount is how many cards of a model the filter covers now.
type GPUModelCount struct {
	Model string `json:"model"`
	Cards int    `json:"cards"`
}

// GPUAnalytics is the GPU utilization over a range.
type GPUAnalytics struct {
	Range  AnalyticsRange `json:"range"`
	Metric string         `json:"metric"` // util | power | temp | mem
	// GroupBy is what the heat map's and the groups' rows are: model, node
	// or gpu.
	GroupBy string     `json:"group_by"`
	Totals  GPUFigures `json:"totals"`
	// CardCount is how many cards the filter covers in the inventory now,
	// ByModel how many of each model.
	CardCount int             `json:"card_count"`
	ByModel   []GPUModelCount `json:"by_model"`
	// Detail says Cards lists each card: only when the filter covers at most
	// a few hundred cards, otherwise Cards is empty.
	Detail bool         `json:"detail"`
	Cards  []GPUCardRow `json:"cards"`
	// Groups are the models or nodes (empty by card: Cards are the rows).
	Groups []GPUGroupRow `json:"groups"`
	Heat   []GPUHeatRow  `json:"heat"`
	// HeatTotal is the heat map's rows in all; when Heat holds fewer, it
	// holds the highest and lowest of the metric over the range.
	HeatTotal int `json:"heat_total"`
}

// GPUTimeRow is a deployment's card time on a model over the range.
type GPUTimeRow struct {
	DeploymentID      uint    `json:"deployment_id"`
	DeploymentName    string  `json:"deployment_name"`
	ServedName        string  `json:"served_name"`
	DeploymentDeleted bool    `json:"deployment_deleted"`
	GPUModel          string  `json:"gpu_model"`
	GPUHours          float64 `json:"gpu_hours"`
	WholeGPUHours     float64 `json:"whole_gpu_hours"`
	AvgInstances      float64 `json:"avg_instances"`
	MaxInstances      int64   `json:"max_instances"`
	// The deployment's tokens over the range (all its models), for the GPU
	// hours per million tokens; null per-million figures without tokens.
	PromptTokens                int64    `json:"prompt_tokens"`
	CompletionTokens            int64    `json:"completion_tokens"`
	GPUHoursPerMillionOutput    *float64 `json:"gpu_hours_per_million_output"`
	GPUHoursPerMillionAllTokens *float64 `json:"gpu_hours_per_million_tokens"`
}

// GPUTimeSeries is a deployment's GPU hours per bucket.
type GPUTimeSeries struct {
	DeploymentID   uint      `json:"deployment_id"`
	DeploymentName string    `json:"deployment_name"`
	GPUHours       []float64 `json:"gpu_hours"`
}

// GPUCapacity is the cluster's cards over the range, in GPU hours.
type GPUCapacity struct {
	GPUModel          string  `json:"gpu_model"`
	TotalGPUHours     float64 `json:"total_gpu_hours"`
	SchedulableHours  float64 `json:"schedulable_gpu_hours"`
	AllocatedGPUHours float64 `json:"allocated_gpu_hours"`
}

// GPUTimeAnalytics is the card time over a range.
type GPUTimeAnalytics struct {
	Range    AnalyticsRange  `json:"range"`
	Rows     []GPUTimeRow    `json:"rows"`
	Series   []GPUTimeSeries `json:"series"`
	Capacity []GPUCapacity   `json:"capacity"`
}

// KeyUsage is one API key's tokens today and over the last 7 days (today
// included), for the API Keys page.
type KeyUsage struct {
	Prefix    string      `json:"prefix"`
	Today     UsageCounts `json:"today"`
	Last7Days UsageCounts `json:"last_7_days"`
}

// KeyUsageList is every key with usage in the last 7 days.
type KeyUsageList struct {
	Items    []KeyUsage `json:"items"`
	TimeZone string     `json:"time_zone"`
}
