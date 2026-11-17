package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/schedactivity"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// opsStats runs the long-term statistics of usage analytics (ADR 0055):
// it hands every finished request, GPU sample and minute of held cards to
// rollup's accumulators, writes their deltas out (token usage to the main
// database every minute, the rest to stats.db every five), rolls finished
// days up, and retains every store by the data retention setting, which it
// applies the moment it changes.
type opsStats struct {
	log   *slog.Logger
	store *store.Store
	stats *statsdb.Store
	obs   *observability
	now   func() time.Time

	requests *rollup.Requests
	gpus     *rollup.GPUs
	gpuTimes *rollup.GPUTimes
	// sched is the scheduling activity (schedactivity.go).
	sched *schedactivity.Service
	// cardModels names each card's model as the inventory last did, for the
	// GPU rows a flush writes; refreshed with every card-time sample.
	cardModels atomic.Pointer[map[rollup.GPUKey]string]

	// fallback is what the configuration asks for, used when the setting
	// row is broken.
	fallback  store.DataRetention
	retention atomic.Pointer[store.DataRetention]
	// liveSinceMS is when live accumulation began (store.UsageRollupState):
	// what ended before it is only known from the one-off fill.
	liveSinceMS int64
	// kick asks the maintenance loop for a pass now (a setting changed).
	kick chan struct{}

	// rolledThrough is the latest day both stores are known to have rolled
	// up; maintenance passes start after it. Only the loop touches it.
	rolledThrough rollup.Day
	// done closes when run has returned; started says run was called.
	done     chan struct{}
	doneOnce sync.Once
	started  atomic.Bool

	mu   sync.Mutex
	last opsPasses
}

// opsPasses are the last maintenance results the status reports.
type opsPasses struct {
	retainAt, rollupAt, flushAt time.Time
	retainErr, rollupErr        string
	flushErr                    string
	// flushRows and flushMS describe the last usage flush, the one whose
	// size grows with the key x deployment combinations in use.
	flushRows int
	flushMS   int64
}

const (
	usageFlushEvery   = time.Minute
	statsFlushEvery   = 5 * time.Minute
	gpuTimeEvery      = time.Minute
	maintainEvery     = 10 * time.Minute
	retainEvery       = time.Hour
	rollupGrace       = 5 * time.Minute // a day rolls up once its last flush is in
	opsStatsFlushTime = 30 * time.Second
)

// newOpsStats opens stats.db, seeds the data retention setting from the
// configuration on a first start, records when live accumulation began and
// hooks the GPU samples of every metrics ingest batch.
func newOpsStats(dataDir string, metricsRetention time.Duration, st *store.Store, obs *observability, log *slog.Logger) (*opsStats, error) {
	stats, err := statsdb.Open(filepath.Join(dataDir, "stats.db"))
	if err != nil {
		return nil, err
	}
	o := &opsStats{
		log: log, store: st, stats: stats, obs: obs, now: time.Now,
		requests: rollup.NewRequests(), gpus: rollup.NewGPUs(), gpuTimes: rollup.NewGPUTimes(),
		fallback: store.DefaultDataRetention(metricsRetention, serverTimeZone()),
		kick:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	current, created, err := st.EnsureDataRetention(o.fallback)
	if err != nil {
		stats.Close()
		return nil, err
	}
	if !created && current.MetricsHours != o.fallback.MetricsHours {
		log.Warn("metrics_retention is only the first start's default; the data retention setting decides",
			"setting_hours", current.MetricsHours, "configured_hours", o.fallback.MetricsHours)
	}
	o.retention.Store(&current)
	state, err := st.UsageRollupState(o.now())
	if err != nil {
		stats.Close()
		return nil, err
	}
	o.liveSinceMS = state.LiveSinceMS
	o.applyRequestLogLimits(current)
	obs.OnSamples(o.observeSamples)
	o.newSchedulingActivity()
	return o, nil
}

// serverTimeZone is the IANA name of the server's zone: TZ, the
// /etc/localtime link, /etc/timezone, or UTC.
func serverTimeZone() string {
	valid := func(name string) bool {
		_, err := time.LoadLocation(name)
		return name != "" && name != "Local" && err == nil
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); valid(tz) {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 && valid(target[i+len("zoneinfo/"):]) {
			return target[i+len("zoneinfo/"):]
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil && valid(strings.TrimSpace(string(b))) {
		return strings.TrimSpace(string(b))
	}
	if name := time.Local.String(); valid(name) {
		return name
	}
	return "UTC"
}

// Retention is the setting in effect.
func (o *opsStats) Retention() store.DataRetention { return *o.retention.Load() }

// SetRetention validates, stores and applies a new setting at once.
func (o *opsStats) SetRetention(r store.DataRetention) error {
	if err := o.store.SetDataRetention(r); err != nil {
		return err
	}
	o.retention.Store(&r)
	o.applyRequestLogLimits(r)
	select {
	case o.kick <- struct{}{}:
	default:
	}
	return nil
}

func (o *opsStats) applyRequestLogLimits(r store.DataRetention) {
	if o.obs != nil && o.obs.UX() != nil {
		o.obs.UX().SetLimits(time.Duration(r.RequestLogDays)*24*time.Hour, int64(r.RequestLogMaxMB)<<20)
	}
}

func (o *opsStats) location() *time.Location {
	loc, err := o.Retention().Location()
	if err != nil {
		return time.UTC
	}
	return loc
}

// observeRequest takes a finished request from the gateway.
func (o *opsStats) observeRequest(r uxrecord.Request) { o.requests.Observe(r) }

// gpuMetrics are the per-card samples GPU utilization counts.
var gpuMetrics = map[string]rollup.GPUMetric{
	"gpuworks_gpu_utilization":         rollup.GPUUtilization,
	"gpuworks_gpu_memory_used_bytes":   rollup.GPUMemoryUsed,
	"gpuworks_gpu_memory_total_bytes":  rollup.GPUMemoryTotal,
	"gpuworks_gpu_power_watts":         rollup.GPUPower,
	"gpuworks_gpu_temperature_celsius": rollup.GPUTemperature,
}

// observeSamples takes a metrics ingest batch on the ingest goroutine and
// counts its card samples.
func (o *opsStats) observeSamples(batch []tsdb.Sample) {
	if !o.Retention().GPU {
		return
	}
	var cards []rollup.GPUSample
	for _, s := range batch {
		metric, ok := gpuMetrics[s.Metric]
		if !ok || s.Labels["instance"] != "" || s.Labels["node"] == "" {
			continue
		}
		index, ok := atoiOK(s.Labels["gpu"])
		if !ok {
			continue
		}
		cards = append(cards, rollup.GPUSample{
			Key:    rollup.GPUKey{NodeUUID: s.Labels["node"], GPUIndex: index, GPUUUID: s.Labels["gpu_uuid"]},
			Metric: metric, Value: s.Value, TimeMS: s.TimeMS,
		})
	}
	o.gpus.Observe(cards)
}

func atoiOK(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// partitionMayShare is whether the scheduler may place a partition on a card
// held outside the platform (planning.PartitionMayShareHeldCard). The
// snapshot's lean columns answer for every other card; only a held card
// whose holders' memory is known reads its node, once per sample, so the
// sampler's cost does not grow with the fleet.
func (o *opsStats) partitionMayShare(c store.GPU, nodes map[uint]*store.Node) bool {
	if _, shared := c.SharesMemoryWithForeign(); !shared {
		return false
	}
	node, ok := nodes[c.NodeID]
	if !ok {
		var err error
		if node, err = o.store.NodeByID(c.NodeID); err != nil {
			node = nil
		}
		nodes[c.NodeID] = node
	}
	if node == nil {
		return false
	}
	for i := range node.GPUs {
		if node.GPUs[i].ID == c.ID {
			return planning.PartitionMayShareHeldCard(node, &node.GPUs[i])
		}
	}
	return false
}

// sampleGPUTime counts the cards held over the last interval and refreshes
// the card models the GPU rows are written with.
func (o *opsStats) sampleGPUTime(at time.Time, interval time.Duration) error {
	r := o.Retention()
	if !r.GPUTime && !r.GPU {
		return nil
	}
	snap, err := o.store.GPUTimeSnapshot()
	if err != nil {
		return err
	}
	models := make(map[rollup.GPUKey]string, len(snap.Cards))
	for _, c := range snap.Cards {
		if node, ok := snap.NodeUUIDs[c.NodeID]; ok {
			models[rollup.GPUKey{NodeUUID: node, GPUIndex: c.Index, GPUUUID: c.UUID}] = c.Name
		}
	}
	o.cardModels.Store(&models)
	if !r.GPUTime {
		return nil
	}
	type cardKey struct {
		node  uint
		ident string
	}
	byIdent := make(map[cardKey]store.GPU, len(snap.Cards))
	nodes := map[uint]*store.Node{}
	cards := make([]rollup.Card, 0, len(snap.Cards))
	for _, c := range snap.Cards {
		byIdent[cardKey{c.NodeID, c.Ident}] = c
		if c.Ownership != "" {
			continue // owned elsewhere: never the platform's to schedule
		}
		// A held card the scheduler may place a partition on by memory
		// (ADR 0068) is capacity the platform schedules on, and its
		// partitions' time is charged to it.
		cards = append(cards, rollup.Card{Model: c.Name, Schedulable: c.CanTakeWork() || o.partitionMayShare(c, nodes)})
	}
	holdings := make([]rollup.Holding, 0, len(snap.Holdings))
	for _, h := range snap.Holdings {
		held := rollup.Holding{DeploymentID: h.DeploymentID}
		for _, ident := range h.Idents {
			card, ok := byIdent[cardKey{h.NodeID, ident}]
			if !ok {
				continue // its node is offline or the card is gone
			}
			share := 1.0
			if h.VRAMLimitBytes > 0 && card.MemoryTotal > 0 {
				share = min(1, float64(h.VRAMLimitBytes)/float64(card.MemoryTotal))
			}
			held.Cards = append(held.Cards, rollup.HeldCard{Model: card.Name, Share: share})
		}
		holdings = append(holdings, held)
	}
	o.gpuTimes.Sample(at, interval, holdings, cards)
	return nil
}

// flushUsage writes the token usage delta; a failed write is given back.
func (o *opsStats) flushUsage(ctx context.Context) error {
	delta := o.requests.TakeUsage()
	if len(delta) == 0 {
		return nil
	}
	started := o.now()
	err := o.store.AddUsage(ctx, delta)
	if err != nil {
		o.requests.GiveBackUsage(delta)
	}
	o.mu.Lock()
	o.last.flushAt, o.last.flushRows, o.last.flushMS = o.now(), len(delta), o.now().Sub(started).Milliseconds()
	o.last.flushErr = errText(err)
	o.mu.Unlock()
	return err
}

// flushStats writes the quality, GPU and card time deltas to stats.db; a
// switched-off statistic's delta is dropped.
func (o *opsStats) flushStats(ctx context.Context) error {
	r := o.Retention()
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if q := o.requests.TakeQuality(); r.Quality && len(q) > 0 {
		if err := o.stats.AddQuality(ctx, q); err != nil {
			o.requests.GiveBackQuality(q)
			keep(err)
		}
	}
	if g := o.gpus.Take(); len(g) > 0 {
		var models map[rollup.GPUKey]string
		if m := o.cardModels.Load(); m != nil {
			models = *m
		}
		if err := o.stats.AddGPUs(ctx, g, models); err != nil {
			o.gpus.GiveBack(g)
			keep(err)
		}
	}
	if t, c := o.gpuTimes.Take(); len(t) > 0 || len(c) > 0 {
		if err := o.stats.AddGPUTime(ctx, t, c); err != nil {
			o.gpuTimes.GiveBack(t, c)
			keep(err)
		}
	}
	return firstErr
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// run is the statistics' loop: flushes, card-time samples, day roll-ups and
// retention, until ctx ends; then a last flush.
func (o *opsStats) run(ctx context.Context) {
	o.started.Store(true)
	// Run may run again after a restart in place; done says the first ended.
	defer o.doneOnce.Do(func() { close(o.done) })
	usageTick := time.NewTicker(usageFlushEvery)
	statsTick := time.NewTicker(statsFlushEvery)
	gpuTick := time.NewTicker(gpuTimeEvery)
	maintainTick := time.NewTicker(maintainEvery)
	defer usageTick.Stop()
	defer statsTick.Stop()
	defer gpuTick.Stop()
	defer maintainTick.Stop()
	var lastRetain time.Time
	maintain := func(force bool) {
		o.rollupDays(ctx)
		if force || o.now().Sub(lastRetain) >= retainEvery {
			lastRetain = o.now()
			o.retain(ctx)
		}
	}
	maintain(true)
	for {
		select {
		case <-ctx.Done():
			o.finalFlush()
			return
		case <-usageTick.C:
			if err := o.flushUsage(ctx); err != nil {
				o.log.Warn("token usage flush failed; kept for the next one", "err", err)
			}
		case <-statsTick.C:
			if err := o.flushStats(ctx); err != nil {
				o.log.Warn("statistics flush failed; kept for the next one", "err", err)
			}
			o.flushScheduling(ctx, false)
		case at := <-gpuTick.C:
			if err := o.sampleGPUTime(at, gpuTimeEvery); err != nil {
				o.log.Warn("card time sample failed", "err", err)
			}
		case <-maintainTick.C:
			maintain(false)
		case <-o.kick:
			maintain(true)
		}
	}
}

// finalFlush writes what accumulated when the server stops.
func (o *opsStats) finalFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), opsStatsFlushTime)
	defer cancel()
	if err := o.flushUsage(ctx); err != nil {
		o.log.Warn("final token usage flush failed", "err", err)
	}
	if err := o.flushStats(ctx); err != nil {
		o.log.Warn("final statistics flush failed", "err", err)
	}
	o.flushScheduling(ctx, true)
}

// rollupDays rolls up every finished day still within the hour retention
// that is not rolled yet: yesterday once its last hour is flushed, and any
// day the server was down at the end of.
func (o *opsStats) rollupDays(ctx context.Context) {
	r := o.Retention()
	loc := o.location()
	now := o.now()
	oldest := rollup.DayOf(now.AddDate(0, 0, -max(r.UsageHourlyDays, r.StatsHourlyDays)), loc)
	today := rollup.DayOf(now, loc)
	if o.rolledThrough >= oldest {
		oldest = o.rolledThrough.Next()
	}
	var firstErr error
	for day := oldest; day < today; day = day.Next() {
		_, end := day.Bounds(loc)
		if now.UnixMilli() < end+rollupGrace.Milliseconds() {
			break
		}
		err := o.rollupDay(ctx, day, loc, false)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if firstErr == nil {
			o.rolledThrough = day
		}
	}
	o.mu.Lock()
	o.last.rollupAt, o.last.rollupErr = now, errText(firstErr)
	o.mu.Unlock()
	if firstErr != nil {
		o.log.Warn("statistics day roll-up failed", "err", firstErr)
	}
}

// rollupDay rolls one day up in both stores; force rolls a day again (the
// fill added hours to it).
//
// A day whose first hours are past a store's hour retention is left as it
// is in that store: its hours are partly gone, and rolling it up again would
// replace its day rows with what is left.
func (o *opsStats) rollupDay(ctx context.Context, day rollup.Day, loc *time.Location, force bool) error {
	r := o.Retention()
	first, _ := day.HoursOf(loc)
	now := o.now()
	usageKept := first >= now.Add(-time.Duration(r.UsageHourlyDays)*24*time.Hour).UnixMilli()
	statsKept := first >= now.Add(-time.Duration(r.StatsHourlyDays)*24*time.Hour).UnixMilli()
	if rolled, err := o.store.UsageDayRolled(ctx, day); err != nil {
		return err
	} else if usageKept && (force || !rolled) {
		if err := o.flushUsage(ctx); err != nil {
			return err
		}
		if err := o.store.RollupUsageDay(ctx, day, loc); err != nil {
			return err
		}
	}
	if rolled, err := o.stats.RolledDay(ctx, day); err != nil {
		return err
	} else if statsKept && (force || !rolled) {
		if err := o.flushStats(ctx); err != nil {
			return err
		}
		o.flushScheduling(ctx, false)
		return o.stats.RollupDay(ctx, day, loc)
	}
	return nil
}

// retain applies the data retention setting to every store.
func (o *opsStats) retain(ctx context.Context) {
	r := o.Retention()
	loc := o.location()
	now := o.now()
	var errs []string
	note := func(what string, err error) {
		if err != nil && ctx.Err() == nil {
			errs = append(errs, what+": "+err.Error())
		}
	}
	_, err := o.store.RetainUsage(ctx, now.Add(-time.Duration(r.UsageHourlyDays)*24*time.Hour).UnixMilli(),
		rollup.DayOf(now.AddDate(0, 0, -r.UsageDailyDays), loc))
	note("token usage", err)
	_, err = o.stats.Retain(ctx, now.Add(-time.Duration(r.StatsHourlyDays)*24*time.Hour).UnixMilli(),
		rollup.DayOf(now.AddDate(0, 0, -r.StatsDailyDays), loc))
	note("statistics", err)
	if o.obs != nil {
		// Both honour ctx between batches; what a stop cuts short resumes on
		// the next pass.
		note("metrics", o.obs.TSDB().Retain(ctx, now.Add(-time.Duration(r.MetricsHours)*time.Hour).UnixMilli()))
		note("logs", o.obs.Logs().Retain(ctx, now.Add(-time.Duration(r.LogsHours)*time.Hour).UnixMilli()))
	}
	o.mu.Lock()
	o.last.retainAt, o.last.retainErr = now, strings.Join(errs, "; ")
	o.mu.Unlock()
	if len(errs) > 0 {
		o.log.Warn("data retention pass failed", "errors", strings.Join(errs, "; "))
	}
}

// close closes stats.db once the loop has made its last flush, after one
// more for the requests the gateway finished while it closed.
func (o *opsStats) close() {
	if o == nil || o.stats == nil {
		return
	}
	if o.started.Load() {
		select {
		case <-o.done:
		case <-time.After(opsStatsFlushTime):
		}
	}
	o.finalFlush()
	if err := o.stats.Close(); err != nil {
		o.log.Warn("stats.db close failed", "err", err)
	}
}
