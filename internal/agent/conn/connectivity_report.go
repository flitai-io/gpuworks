package conn

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
)

var connectionStages = []string{"configuration", "dial", "dns", "connect", "proxy_connect", "tls", "ws_upgrade", "yamux", "hello", "wait_registered", "server_hello", "session"}
var connectionAttemptSequence atomic.Uint64

type connectionPhase struct {
	state             string
	started, observed time.Time
	elapsed           time.Duration
}

// connectionObservation contains only bounded, safe observations. Credentials
// remain in Credentials; no raw error, response, token or session ID is copied.
type connectionObservation struct {
	mu                                     sync.Mutex
	initialized                            bool
	facts                                  map[string]string
	phases                                 map[string]connectionPhase
	active, attempt                        string
	dataEnabled, negotiated, eventRecorded bool
	strictProbe, registered                bool
}

func safeConnectionURL(raw, token string, originOnly bool) string {
	if raw == "" || len(raw) > 2048 || agentconf.ValidateServer(raw) != nil {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if token != "" && (strings.Contains(raw, token) || strings.Contains(u.Path, token)) {
		return ""
	}
	if strings.IndexFunc(raw, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return ""
	}
	if originOnly {
		u.Path = ""
		u.RawPath = ""
	}
	return u.String()
}

func safeProxyURL(u *url.URL, token string) string {
	if u == nil {
		return ""
	}
	// Proxy credentials, paths and query values are never passed to reports.
	origin := &url.URL{Scheme: u.Scheme, Host: u.Host}
	if origin.Scheme != "http" && origin.Scheme != "https" && origin.Scheme != "socks5" && origin.Scheme != "socks5h" {
		return ""
	}
	if origin.Host == "" || strings.ContainsAny(origin.Host, " \t\r\n") || (token != "" && strings.Contains(origin.Host, token)) {
		return ""
	}
	return origin.String()
}

func (c *Conn) initConnectionObservation() {
	o := &c.observation
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.initialized {
		return
	}
	o.initialized = true
	source, serverSource := "agent_runtime", "agent_runtime"
	if c.creds.Smoke {
		source, serverSource = "cli", "unknown"
	}
	mode := c.creds.ConnectMode
	if mode == "" {
		mode = agentconf.AgentDial
	}
	o.facts = map[string]string{"source": source, "server_source": serverSource, "token_present": strconv.FormatBool(strings.TrimSpace(c.creds.NodeToken) != ""), "control": "unknown", "data_lane": "unknown", "outcome": "unknown", "reason": "none", "proxy": "unknown"}
	if mode == agentconf.AgentDial || mode == agentconf.ServerDial {
		o.facts["connect_mode"] = mode
	}
	if target := safeConnectionURL(c.creds.ServerURL, c.creds.NodeToken, false); target != "" {
		o.facts["server_url"] = target
	}
	o.dataEnabled = c.creds.DataLane
	o.phases = make(map[string]connectionPhase, len(connectionStages))
	for _, name := range connectionStages {
		o.phases[name] = connectionPhase{state: "not_observed"}
	}
	o.phases["configuration"] = connectionPhase{state: "not_checked"}
	o.attempt = fmt.Sprintf("%016x%016x", time.Now().UnixNano(), connectionAttemptSequence.Add(1))
	if c.creds.Smoke {
		o.facts["data_lane"] = "unverified"
	}
	if mode == agentconf.ServerDial {
		o.facts["data_lane"] = "not_applicable"
		for _, name := range []string{"dial", "dns", "connect", "proxy_connect", "tls", "ws_upgrade"} {
			o.phases[name] = connectionPhase{state: "not_applicable"}
		}
	}
}

func (c *Conn) beginConnectionPhase(name string, active bool) {
	c.initConnectionObservation()
	o := &c.observation
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.phases[name]; !ok {
		return
	}
	o.phases[name] = connectionPhase{state: "in_progress", started: time.Now()}
	if active {
		o.active = name
	}
}

func (c *Conn) endConnectionPhase(name string, err error) {
	c.initConnectionObservation()
	o := &c.observation
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.phases[name]
	if !ok {
		return
	}
	// A canceled happy-eyeballs loser must not undo a successful transport.
	if name == "connect" && p.state == "ok" && err != nil {
		return
	}
	p.state = "ok"
	if err != nil {
		p.state = connectionErrorState(err)
	}
	p.observed = time.Now()
	if !p.started.IsZero() {
		p.elapsed = p.observed.Sub(p.started)
	}
	o.phases[name] = p
}

func (c *Conn) observationFact(name, value string) {
	c.initConnectionObservation()
	o := &c.observation
	o.mu.Lock()
	defer o.mu.Unlock()
	if value == "" {
		delete(o.facts, name)
	} else {
		o.facts[name] = value
	}
}

// ConnectionReport returns current observations only. It does not probe, read
// logs, or borrow mutable maps. checked_at is acquisition time, not heartbeat.
func (c *Conn) ConnectionReport() proto.DoctorReport {
	c.initConnectionObservation()
	c.dataMu.Lock()
	attached := c.dataSess != nil && !c.dataSess.IsClosed()
	everAttached := c.dataEver
	c.dataMu.Unlock()
	o := &c.observation
	o.mu.Lock()
	defer o.mu.Unlock()
	values := make(map[string]string, len(o.facts)+len(o.phases)*3+1)
	for k, v := range o.facts {
		values[k] = v
	}
	values["checked_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if values["source"] == "agent_runtime" && (values["connect_mode"] != "server_dial" || o.dataEnabled) {
		switch {
		case !o.dataEnabled:
			values["data_lane"] = "disabled"
		case values["control"] != "connected":
			values["data_lane"] = "pending"
		case !o.negotiated:
			values["data_lane"] = "not_negotiated"
		case attached:
			values["data_lane"] = "attached"
		case !everAttached:
			values["data_lane"] = "pending"
		default:
			values["data_lane"] = "detached"
		}
	}
	for name, p := range o.phases {
		values["phase."+name] = p.state
		if !p.observed.IsZero() {
			values["observed_at."+name] = p.observed.UTC().Format(time.RFC3339Nano)
		}
		if !p.started.IsZero() && !p.observed.IsZero() {
			values["elapsed_ms."+name] = strconv.FormatInt(max(p.elapsed.Milliseconds(), 0), 10)
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	report := proto.DoctorReport{}
	var text strings.Builder
	for _, k := range keys {
		report.Facts = append(report.Facts, proto.DoctorFact{Code: "connection." + k, Value: values[k]})
		fmt.Fprintf(&text, "%s: %s\n", k, values[k])
	}
	report.Report = text.String()
	return report
}

func (c *Conn) connectionReady() {
	c.initConnectionObservation()
	o := &c.observation
	o.mu.Lock()
	o.negotiated = c.sessionID != ""
	o.facts["outcome"] = "ok"
	o.facts["reason"] = "none"
	if c.creds.Smoke {
		o.facts["control"] = "unknown"
		o.facts["outcome"] = "unknown"
		if o.registered {
			o.facts["control"] = "smoke_verified"
			o.facts["outcome"] = "ok"
		}
	} else {
		o.facts["control"] = "connected"
	}
	// A returned token is credential material too, even if a hostile peer made
	// it coincide with a configured URL path.
	if token := c.creds.NodeToken; token != "" {
		// Reproject only previously safe values. Never reconstruct an original
		// value already withheld for the original credential.
		if target := safeConnectionURL(o.facts["server_url"], token, false); target != "" {
			o.facts["server_url"] = target
		} else {
			delete(o.facts, "server_url")
		}
		proxy, err := url.Parse(o.facts["proxy_url"])
		if err == nil {
			if origin := safeProxyURL(proxy, token); origin != "" {
				o.facts["proxy_url"] = origin
			} else {
				delete(o.facts, "proxy_url")
			}
		} else {
			delete(o.facts, "proxy_url")
		}
	}
	o.mu.Unlock()
	if !c.creds.Smoke {
		c.beginConnectionPhase("session", true)
	}
}

// RecordReconnect records the actual chosen delay; it never changes policy or
// sleeps. Only the first record for this failed control attempt emits an event.
func (c *Conn) RecordReconnect(delay time.Duration) {
	c.initConnectionObservation()
	o := &c.observation
	o.mu.Lock()
	if delay < 0 {
		delay = 0
	}
	o.facts["retry_at"] = time.Now().Add(delay).UTC().Format(time.RFC3339Nano)
	emit := !o.eventRecorded && o.facts["source"] == "agent_runtime" && o.facts["outcome"] == "failed"
	if emit {
		o.eventRecorded = true
	}
	o.mu.Unlock()
	if emit {
		c.emitConnectionEvent(c.connectionLane())
	}
}

func (c *Conn) connectionLane() string {
	if !c.creds.DataLane {
		return "single"
	}
	if c.creds.ConnectMode == agentconf.ServerDial {
		c.observation.mu.Lock()
		negotiated := c.observation.negotiated
		c.observation.mu.Unlock()
		if !negotiated {
			return "single"
		}
	}
	return "ctrl"
}

func (c *Conn) emitConnectionEvent(lane string) {
	if c.log == nil {
		return
	}
	o := &c.observation
	o.mu.Lock()
	stage := o.active
	if stage == "" {
		stage = "dial"
	}
	at := o.phases[stage].observed
	if at.IsZero() {
		at = time.Now()
	}
	args := []any{"source", "agent_runtime", "stage", stage, "outcome", "failed", "reason", o.facts["reason"], "attempt", o.attempt, "lane", lane, "observed_at", at.UTC().Format(time.RFC3339Nano)}
	if value := o.facts["server_url"]; value != "" {
		args = append(args, "server_url", value)
	}
	if value := o.facts["retry_at"]; value != "" {
		args = append(args, "retry_at", value)
	}
	o.mu.Unlock()
	c.log.Info("connection_event_v1", args...)
}

func (c *Conn) observationFailureStage(stage string) {
	c.observation.mu.Lock()
	defer c.observation.mu.Unlock()
	if _, ok := c.observation.phases[stage]; ok {
		c.observation.active = stage
	}
}

func (c *Conn) emitDataConnectionFailure(err error, retry time.Time) {
	if c.log == nil || err == nil {
		return
	}
	stage := "dial"
	attempt := ""
	var failure *connectionFailure
	if errors.As(err, &failure) {
		if failure.stage != "" {
			stage = failure.stage
		}
		attempt = failure.attempt
	}
	if attempt == "" {
		attempt = fmt.Sprintf("%016x%016x", time.Now().UnixNano(), connectionAttemptSequence.Add(1))
	}
	o := &c.observation
	o.mu.Lock()
	target := o.facts["server_url"]
	o.mu.Unlock()
	args := []any{"source", "agent_runtime", "stage", stage, "outcome", "failed", "reason", connectionReason(err), "attempt", attempt, "lane", "data", "observed_at", time.Now().UTC().Format(time.RFC3339Nano)}
	if target != "" {
		args = append(args, "server_url", target)
	}
	if !retry.IsZero() {
		args = append(args, "retry_at", retry.UTC().Format(time.RFC3339Nano))
	}
	c.log.Info("connection_event_v1", args...)
}

func (c *Conn) strictConnectivityProbe() bool {
	c.observation.mu.Lock()
	defer c.observation.mu.Unlock()
	return c.observation.strictProbe
}
