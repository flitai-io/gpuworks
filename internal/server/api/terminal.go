package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/webterm"
)

// Browser-facing control messages. Terminal bytes travel as binary WebSocket
// messages; anything structured travels as JSON text. Splitting them this way
// means the frontend never parses a byte header, and the translation to and from
// the agent's frame format stays here.
type termClientMsg struct {
	Type string `json:"type"` // resize
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type termServerMsg struct {
	Type    string `json:"type"` // info | exit | error
	User    string `json:"user,omitempty"`
	UID     *int   `json:"uid,omitempty"`
	Shell   string `json:"shell,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Host    string `json:"host,omitempty"`
	Code    *int32 `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

const (
	// termMaxClientMsg caps a single inbound message. Keystrokes and pastes are
	// small; anything larger is a client bug or an attempt to push the agent
	// around.
	termMaxClientMsg = 1 << 20
)

var (
	errTerminalUnavailable = staticErr("terminal streaming is not available")
	errNodeOffline         = staticErr("node is not connected")
	errAgentNoTerminal     = staticErr("this node's agent does not support terminals; upgrade the agent")
	errTerminalTargetRef   = staticErr("container terminals require a container id")
	// Kept for old agents that advertise no controlled-shell capability.
	errUdockerNoContainerTerm = staticErr("runveil container shell is unavailable on this agent")
	errAgentNoContainerTerm   = staticErr("this node's agent does not support container terminals")
	errUdockerCapability      = staticErr("runveil container shell is unavailable on this agent")
)

// nodeTerminal upgrades to a WebSocket and bridges it to a pty on the node.
//
// Admission happens entirely before the upgrade: once the connection is
// hijacked there is no way to send an HTTP status, so a rejected session would
// otherwise have to be reported as a WebSocket close code that browsers surface
// poorly. Everything that can say "no" says it while this is still plain HTTP.
func (a *apiHandlers) nodeTerminal(w http.ResponseWriter, r *http.Request) {
	id, authErr := a.authenticateIdentity(r)
	if authErr != nil {
		// The route also carries managementAdmin; this is belt and braces for a
		// feature that hands out a root shell.
		a.writeManagementAuthError(w, authErr)
		return
	}
	if a.d.OpenTerminal == nil || a.d.Terminal == nil {
		writeErr(w, http.StatusServiceUnavailable, errTerminalUnavailable)
		return
	}

	// The kill switch is checked before anything about the node, so turning the
	// feature off reports exactly that rather than an incidental complaint about
	// the node being offline or its agent being old. Manager.Begin checks it
	// again; this is for precedence and a clear message, not for safety.
	if !a.d.Store.TerminalSettings().Enabled {
		writeErr(w, http.StatusForbidden, webterm.ErrDisabled)
		return
	}

	uuid := chi.URLParam(r, "uuid")
	node, err := a.d.Store.NodeByUUID(uuid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if a.d.Online != nil && !a.d.Online(node.UUID) {
		writeErr(w, http.StatusServiceUnavailable, errNodeOffline)
		return
	}
	// Agents predating the web terminal do not advertise webterm. Refusing
	// here produces a clear message instead of a terminal that connects and
	// then goes blank.
	if a.d.NodeCaps != nil && !hasCap(a.d.NodeCaps(node.UUID), proto.CapWebterm) {
		writeErr(w, http.StatusServiceUnavailable, errAgentNoTerminal)
		return
	}

	target := r.URL.Query().Get("target")
	if target == "" {
		target = proto.TermTargetHost
	}
	ref := r.URL.Query().Get("ref")
	switch target {
	case proto.TermTargetHost:
		ref = "" // a host pty has nothing to reference
	case proto.TermTargetContainer:
		if ref == "" {
			writeErr(w, http.StatusBadRequest, errTerminalTargetRef)
			return
		}
		// Refuse here rather than letting the agent reject it, so this does not
		// consume an audit record and a concurrency slot on the way to a failure.
		// udocker advertises a separate capability because its implementation is a
		// controlled shell in the workload rootfs, not a Docker-style attach.
		cap := proto.CapWebtermContainer
		if isUdockerRuntime(node.RuntimeDriver) {
			cap = proto.CapWebtermContainerUdocker
		}
		if a.d.NodeCaps == nil || !hasCap(a.d.NodeCaps(node.UUID), cap) {
			if isUdockerRuntime(node.RuntimeDriver) {
				writeErr(w, http.StatusBadRequest, errUdockerNoContainerTerm)
			} else {
				writeErr(w, http.StatusServiceUnavailable, errAgentNoContainerTerm)
			}
			return
		}
	default:
		writeProductError(w, http.StatusBadRequest, ErrorTerminalTargetUnsupported, "unsupported terminal target: "+target, map[string]any{"target": target})
		return
	}

	rows := termDimension(r.URL.Query().Get("rows"), 24)
	cols := termDimension(r.URL.Query().Get("cols"), 80)

	sess, err := a.d.Terminal.Begin(webterm.Request{
		UserID:   id.claims.UserID,
		Username: id.claims.Username,
		NodeUUID: node.UUID,
		Target:   target,
		Ref:      ref,
		ClientIP: clientIP(r),
	})
	if err != nil {
		writeErr(w, terminalStatus(err), err)
		return
	}

	// Origin verification is deliberately left on. The hub skips it because its
	// peers are agents rather than browsers; here the peer is a browser, and
	// skipping it would allow cross-site WebSocket hijacking of a root shell.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		_ = sess.Close(webterm.ReasonServerError)
		a.d.Log.Warn("terminal ws accept failed", "node", node.UUID, "err", err)
		return
	}
	c.SetReadLimit(termMaxClientMsg)

	stream, err := a.d.OpenTerminal(node.UUID, target, ref, rows, cols, int(sess.Grace.Seconds()))
	if err != nil {
		writeTermServerMsg(r.Context(), c, termServerMsg{Type: "error", Message: err.Error()})
		_ = c.Close(websocket.StatusInternalError, "open terminal")
		_ = sess.Close(webterm.ReasonNodeGone)
		return
	}

	var changes <-chan bus.Event
	if a.d.Bus != nil {
		sub := a.d.Bus.Subscribe(8, "management-session")
		defer sub.Close()
		changes = sub.C()
	}
	reason := bridgeTerminal(r.Context(), c, stream, sess, func() bool { return a.managementStreamAllowed(r, true) }, changes)
	_ = stream.Close()
	_ = sess.Close(reason)
	_ = c.Close(websocket.StatusNormalClosure, "session ended")
}

// termWriter serializes WebSocket writes. The output pump, the idle notice and
// the error path all write from different goroutines, and coder/websocket allows
// only one writer at a time.
type termWriter struct {
	mu sync.Mutex
	c  *websocket.Conn
}

func (tw *termWriter) write(ctx context.Context, typ websocket.MessageType, b []byte) error {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	return tw.c.Write(ctx, typ, b)
}

// bridgeTerminal pumps bytes both ways until one side ends, and reports the
// close reason for the audit record.
func bridgeTerminal(ctx context.Context, c *websocket.Conn, stream net.Conn, sess *webterm.Session, authorized func() bool, changes <-chan bus.Event) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tw := &termWriter{c: c}

	// Agent to browser.
	agentDone := make(chan string, 1)
	go func() {
		agentDone <- pumpAgentToClient(ctx, tw, stream, sess)
	}()

	// Browser to agent.
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		pumpClientToAgent(ctx, c, stream, sess)
	}()

	// Keep database-backed revocation checks outside the terminal I/O pumps.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-changes:
			if !authorized() {
				return webterm.ReasonClientClosed
			}
		case <-ticker.C:
			if !authorized() {
				return webterm.ReasonClientClosed
			}
		case reason := <-agentDone:
			// A browser close tears down the peer stream too, so the agent pump can
			// observe EOF at the same time as the client reader. Prefer the client
			// cause when both are already complete; otherwise an ordinary tab close
			// is mis-audited as a node/shell disconnect.
			select {
			case <-clientDone:
				return webterm.ReasonClientClosed
			default:
			}
			return reason
		case <-clientDone:
			// The tab closed or the socket broke. Closing the stream ends the agent's
			// pty, which reaps the whole terminal session on the node.
			return webterm.ReasonClientClosed
		case <-sess.Expired():
			// An unattended root shell is the risk this timeout exists for.
			writeTermServerMsg(ctx, c, termServerMsg{
				Type: "exit", Reason: "idle timeout", Code: intPtr32(-1),
			})
			return webterm.ReasonIdleTimeout
		case <-ctx.Done():
			return webterm.ReasonClientClosed
		}
	}
}

// pumpAgentToClient forwards pty output and control frames to the browser.
func pumpAgentToClient(ctx context.Context, tw *termWriter, stream net.Conn, sess *webterm.Session) string {
	fr := proto.NewTermFrameReader(stream)
	for {
		f, err := fr.Next()
		if err != nil {
			// EOF here means the agent closed the stream, which for a pty means
			// the shell is gone.
			return webterm.ReasonShellExited
		}
		switch f.Type {
		case proto.TermData:
			sess.AddOut(len(f.Payload))
			if err := tw.write(ctx, websocket.MessageBinary, f.Payload); err != nil {
				return webterm.ReasonClientClosed
			}
		case proto.TermInfo:
			var info proto.TermSessionInfo
			if json.Unmarshal(f.Payload, &info) != nil {
				continue
			}
			uid := info.UID
			// Agents run as root under systemd but as the installing user via the
			// nohup fallback, so the UI states what the shell actually is instead
			// of assuming.
			writeTermMsg(ctx, tw, termServerMsg{
				Type: "info", User: info.User, UID: &uid,
				Shell: info.Shell, Cwd: info.Cwd, Host: info.Host,
			})
		case proto.TermExit:
			code, reason, err := proto.DecodeExit(f.Payload)
			if err != nil {
				return webterm.ReasonShellExited
			}
			writeTermMsg(ctx, tw, termServerMsg{Type: "exit", Code: &code, Reason: reason})
			return webterm.ReasonShellExited
		default:
			// Ignore unknown frame types so a newer agent stays compatible.
		}
	}
}

// pumpClientToAgent forwards keystrokes and resize requests to the agent.
func pumpClientToAgent(ctx context.Context, c *websocket.Conn, stream net.Conn, sess *webterm.Session) {
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			// Only operator input refreshes the idle deadline. Keying it on output
			// would let `watch nvidia-smi` hold a root shell open forever.
			sess.Touch()
			sess.AddIn(len(data))
			if err := proto.WriteTermData(stream, data); err != nil {
				return
			}
		case websocket.MessageText:
			var msg termClientMsg
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			if msg.Type != "resize" {
				continue
			}
			// A resize is the browser reporting geometry, not the operator working,
			// so it does not refresh the idle deadline.
			if err := proto.WriteTermFrame(stream, proto.TermResize, proto.EncodeResize(msg.Rows, msg.Cols)); err != nil {
				return
			}
		}
	}
}

func writeTermMsg(ctx context.Context, tw *termWriter, msg termServerMsg) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_ = tw.write(ctx, websocket.MessageText, b)
}

// writeTermServerMsg writes one control message on a connection with no shared
// writer yet (before or instead of the pumps).
func writeTermServerMsg(ctx context.Context, c *websocket.Conn, msg termServerMsg) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, b)
}

// terminalStatus maps an admission failure to a status code.
func terminalStatus(err error) int {
	switch {
	case errors.Is(err, webterm.ErrDisabled):
		return http.StatusForbidden
	case errors.Is(err, webterm.ErrNodeBusy), errors.Is(err, webterm.ErrClusterBusy):
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// termDimension parses a window dimension, falling back to def. Values are
// clamped rather than rejected: a bad size should not deny a session, and the
// pty needs something sane regardless.
func termDimension(s string, def uint16) uint16 {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n > 1000 {
		n = 1000
	}
	return uint16(n)
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// clientIP returns the peer address without its port, for the audit record. The
// forwarded headers are deliberately ignored: they are client-controlled, and an
// audit trail should record the address the server actually saw.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func intPtr32(v int32) *int32 { return &v }

// terminalSettings reports whether a terminal can be opened on this node, and
// why not when it cannot.
//
// This preflight exists because browsers do not expose the HTTP status of a
// failed WebSocket handshake: without it, "disabled by an admin", "node
// offline", "agent too old" and "limit reached" would all reach the operator as
// an indistinguishable connection failure.
func (a *apiHandlers) terminalSettings(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	cfg := a.d.Store.TerminalSettings()
	total, node := 0, 0
	if a.d.Terminal != nil {
		total, node = a.d.Terminal.Active(uuid)
	}

	canOpen, reason := true, ""
	switch {
	case a.d.OpenTerminal == nil || a.d.Terminal == nil:
		canOpen, reason = false, errTerminalUnavailable.Error()
	case !cfg.Enabled:
		canOpen, reason = false, webterm.ErrDisabled.Error()
	case a.d.Online != nil && !a.d.Online(uuid):
		canOpen, reason = false, errNodeOffline.Error()
	case a.d.NodeCaps != nil && !hasCap(a.d.NodeCaps(uuid), proto.CapWebterm):
		canOpen, reason = false, errAgentNoTerminal.Error()
	case total >= cfg.MaxGlobal:
		canOpen, reason = false, webterm.ErrClusterBusy.Error()
	case node >= cfg.MaxPerNode:
		canOpen, reason = false, webterm.ErrNodeBusy.Error()
	}

	// Container terminals are runtime-specific. Docker has a true exec/attach;
	// udocker advertises a controlled shell that starts a fresh proot process and
	// reconstructs the workload context from its persisted spec.
	containerSupported, containerReason, containerRuntime, containerCapability := a.containerTerminalPreflight(uuid)

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":              cfg.Enabled,
		"idle_timeout_min":     cfg.IdleTimeoutMin,
		"max_per_node":         cfg.MaxPerNode,
		"max_global":           cfg.MaxGlobal,
		"active_total":         total,
		"active_node":          node,
		"can_open":             canOpen,
		"reason":               reason,
		"container_supported":  containerSupported,
		"container_reason":     containerReason,
		"container_runtime":    containerRuntime,
		"container_capability": containerCapability,
	})
}

// containerTerminalPreflight is deliberately runtime-specific. A generic
// webterm.container bit is sufficient for runtimes with a true attach/exec
// primitive (for example Docker). udocker uses its own bit because the shell is
// simulated through a fresh proot invocation with explicit workload context;
// the UI can explain that relationship without pretending it is an attach.
func (a *apiHandlers) containerTerminalPreflight(uuid string) (supported bool, reason, runtimeName, capability string) {
	n, err := a.d.Store.NodeByUUID(uuid)
	if err != nil {
		return false, errNodeOffline.Error(), "", proto.CapWebtermContainer
	}
	runtimeName = strings.ToLower(strings.TrimSpace(n.RuntimeDriver))
	if runtimeName == "" {
		runtimeName = "unknown"
	}
	if isUdockerRuntime(runtimeName) {
		capability = proto.CapWebtermContainerUdocker
		if a.d.NodeCaps == nil || !hasCap(a.d.NodeCaps(uuid), proto.CapWebtermContainerUdocker) {
			return false, errUdockerCapability.Error(), runtimeName, capability
		}
		return true, "", runtimeName, capability
	}
	capability = proto.CapWebtermContainer
	if a.d.NodeCaps == nil || !hasCap(a.d.NodeCaps(uuid), proto.CapWebtermContainer) {
		return false, errAgentNoContainerTerm.Error(), runtimeName, capability
	}
	return true, "", runtimeName, capability
}

func isUdockerRuntime(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "udocker")
}

// terminalSessionsList returns recent audit records (metadata only; no terminal
// content is ever stored).
func (a *apiHandlers) terminalSessionsList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.d.Store.TerminalSessions(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]dto.TerminalSession, 0, len(rows))
	for _, s := range rows {
		out = append(out, terminalSessionDTO(s))
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func terminalSessionDTO(s store.TerminalSession) dto.TerminalSession {
	out := dto.TerminalSession{
		ID: s.ID, Username: s.Username, NodeUUID: s.NodeUUID, Target: s.Target, Ref: s.Ref, ClientIP: s.ClientIP,
		StartedAt: s.StartedAt, BytesIn: s.BytesIn, BytesOut: s.BytesOut, Active: s.EndedAt == nil,
	}
	if s.EndedAt != nil {
		ended := *s.EndedAt
		out.EndedAt = &ended
		out.DurationSec = int64(s.EndedAt.Sub(s.StartedAt) / time.Second)
		out.CloseReason = s.CloseReason
	}
	return out
}
