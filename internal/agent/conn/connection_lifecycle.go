package conn

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/agent/sysinfo"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/version"
	"gpuworks.ai/gpuworks/internal/wsconn"
)

// Run dials, handshakes, and serves until the connection drops or ctx is done.
// It returns the terminating error (nil on clean ctx cancellation). A smoke
// connection's error names the stage that was in progress.
func (c *Conn) Run(ctx context.Context) (err error) {
	defer func() { err = c.finishConnection(ctx, err) }()
	c.beginConnectionPhase("configuration", true)
	// Observe validation without changing the normal/legacy dial path. The
	// dedicated diagnostic entry validates strictly before it calls Run.
	c.endConnectionPhase("configuration", validateConnectionCredentials(c.creds))
	c.observationFact("control", "probing")
	c.observationFact("outcome", "probing")
	if c.creds.Smoke {
		c.trace = newSmokeTrace()
		defer func() { err = c.finishSmoke(err) }()
	}
	// A connection that will open a data lane dials the ctrl lane with the ctrl
	// profile and ?lane=ctrl; the server then issues a SessionID. Otherwise the
	// agent keeps the single adaptive data-profile session with no lane
	// parameter, so an old server sees exactly the request it expects.
	wantData := c.wantsDataLane()
	lane := proto.LaneData
	wsURL := toWSURL(c.creds.ServerURL) + "/api/v1/agent/connect"
	if wantData {
		lane = proto.LaneCtrl
		wsURL += "?lane=ctrl"
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client, dialedFrom, transportTrace := c.observedControlClient()
	dialCtx = transportTrace.context(dialCtx)
	endDial := c.smokeStage("dial")
	//nolint:bodyclose // coder/websocket.Dial owns handshake bodies; NetConn below owns the upgraded connection.
	ws, response, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"User-Agent": []string{"gpuworks-agent/" + version.Version}},
	})
	endDial()
	if err == nil {
		transportTrace.done("ws_upgrade", nil)
	} else if response != nil {
		transportTrace.done("ws_upgrade", err)
		err = &connectionFailure{reason: "ws_upgrade", cause: err}
	}
	if err != nil {
		c.endConnectionPhase("dial", err)
		c.observationFailureStage(transportTrace.current())
		return err
	}
	ws.SetReadLimit(-1) // yamux frames can exceed the default 32KiB limit
	netConn := wsconn.Wrap(websocket.NetConn(context.Background(), ws, websocket.MessageBinary))
	defer netConn.Close()
	c.setLocal(dialedFrom(), proxiedServer(c.creds.ServerURL))
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			_ = netConn.Close()
		case <-stopClose:
		}
	}()

	return c.runYamuxClient(ctx, netConn, lane, wantData)
}

// wantsDataLane reports whether this connection should open a dedicated data
// lane: the escape hatch is on, it is not a smoke probe, and it is an agent_dial
// connection; server_dial negotiates its data lane at the TCP listener.
func (c *Conn) wantsDataLane() bool {
	return c.creds.DataLane && !c.creds.Smoke && c.creds.ConnectMode != "server_dial"
}

// RunAccepted authenticates a server-initiated raw TCP connection, then serves
// the same yamux ctrl protocol used by the normal agent_dial WebSocket path.
func (c *Conn) RunAccepted(ctx context.Context, netConn net.Conn) (err error) {
	defer netConn.Close()
	stop := context.AfterFunc(ctx, func() { _ = netConn.Close() })
	defer stop()
	proof, err := c.authenticateServerDial(netConn)
	if err != nil {
		c.endConnectionPhase("hello", err)
		return c.finishConnection(ctx, err)
	}
	if proof.Lane == proto.LaneData {
		return errors.New("data lane requires an active parent control session")
	}
	return c.runAcceptedControl(ctx, netConn, proof)
}

func (c *Conn) runAcceptedControl(ctx context.Context, netConn net.Conn, proof proto.ServerDialProof) (err error) {
	defer func() { err = c.finishConnection(ctx, err) }()
	defer c.closeDataLane()
	c.observationFact("connect_mode", "server_dial")
	c.setLocal(netConn.LocalAddr(), false)
	lane := proto.LaneData
	if proof.Lane == proto.LaneCtrl {
		lane = proto.LaneCtrl
	}
	return c.runYamuxClient(ctx, netConn, lane, false)
}

func (c *Conn) runYamuxClient(ctx context.Context, netConn net.Conn, lane string, wantData bool) error {
	// The ctrl connection uses the profile selected by lane; a data lane, when
	// opened, always uses the adaptive data profile on its own socket.
	endYamux := c.smokeStage("yamux")
	ycfg := laneYamuxConfig(lane, io.Discard)
	var err error
	c.ysess, err = yamux.Client(netConn, ycfg, nil)
	if err != nil {
		endYamux()
		return err
	}
	defer c.ysess.Close()

	// Open the ctrl stream and write its header.
	c.ctrl, err = c.ysess.OpenStream(ctx)
	if err != nil {
		endYamux()
		return err
	}
	if err := c.writeStreamHeader(proto.StreamHeader{Kind: proto.StreamCtrl}); err != nil {
		endYamux()
		return err
	}
	endYamux()
	c.dec = json.NewDecoder(bufio.NewReader(c.ctrl))

	if err := c.handshake(); err != nil {
		return err
	}
	c.connectionReady()
	if c.creds.Smoke {
		return nil
	}
	c.log.Info("connected to server")
	c.confirmUpgrade()

	// Bind the persistent handlers to this connection's sender so the engine and
	// downloader can report status; unbind when the connection ends.
	if c.handlers != nil {
		c.handlers.Bind(c.handlerSend(ctx))
		defer c.handlers.Unbind()
	}

	// Accept server-initiated streams (tunnel for /v1 proxy, logtail).
	go c.acceptStreams(ctx)

	// Open the data lane only when the server issued a SessionID (a current
	// server that accepted lane=ctrl). It runs in its own goroutine and reconnects
	// independently, never blocking the ctrl loops; it is cancelled when this
	// connection ends, which closes the data lane (a child of the ctrl session).
	if wantData && c.sessionID != "" {
		dctx, dcancel := context.WithCancel(ctx)
		defer dcancel()
		defer c.closeDataLane()
		go c.runDataLane(dctx)
	}

	// Report initial status immediately, then run the periodic loops.
	c.reportStatus(ctx)
	return c.serve(ctx)
}

func (c *Conn) handshake() error {
	endHello := c.smokeStage("hello")
	hello := proto.Hello{
		ProtoMin: proto.ProtoMin,
		ProtoMax: proto.ProtoMax,
		Version:  version.Version,
		Caps:     helloCapabilitiesForRuntime(c.prober, runtimeName(c.handlers)),
	}
	if h, ok := c.handlers.(containerTerminalCapabilities); ok {
		hello.Caps = append(hello.Caps, validatedContainerCaps(runtimeName(c.handlers), h.ContainerTerminalCapabilities())...)
	}
	hello.Caps = c.creds.Simulated.Capabilities(hello.Caps)
	if h, ok := c.handlers.(runtimeAssetCache); ok {
		hello.RuntimeAssetDigests = h.CachedRuntimeAssetDigests()
	}
	if c.creds.NodeToken != "" {
		hello.NodeUUID = c.creds.NodeUUID
		hello.NodeToken = c.creds.NodeToken
	}
	hello.Hostname = c.creds.Simulated.SysInfo(sysinfo.Collect()).Hostname
	if h, ok := c.handlers.(interface{ OwnModelsDir() string }); ok {
		hello.OwnModelsDir = h.OwnModelsDir()
	}
	hello.ConnectMode = c.creds.ConnectMode
	hello.Smoke = c.creds.Smoke
	if err := c.send(proto.THello, 0, hello); err != nil {
		endHello()
		return err
	}
	endHello()

	// First reply is the registered identity, followed by the server hello.
	endRegistered := c.smokeStage("wait_registered")
	var env proto.Envelope
	if err := c.dec.Decode(&env); err != nil {
		endRegistered()
		return err
	}
	endRegistered()
	if env.T != proto.TRegistered && env.T != proto.TReject && c.strictConnectivityProbe() {
		return errors.New("registration response required for connection diagnosis")
	}
	if env.T == proto.TReject {
		var rej proto.Reject
		if err := json.Unmarshal(env.D, &rej); err != nil {
			return err
		}
		return &RejectError{Reason: rej.Reason, Detail: rej.Detail}
	}
	if env.T == proto.TRegistered {
		var reg proto.Registered
		if err := json.Unmarshal(env.D, &reg); err != nil {
			return err
		}
		if c.strictConnectivityProbe() && strings.TrimSpace(reg.NodeUUID) == "" {
			return errors.New("registered identity required for connection diagnosis")
		}
		c.observation.mu.Lock()
		c.observation.registered = true
		c.observation.mu.Unlock()
		c.creds.NodeUUID = reg.NodeUUID
		if strings.TrimSpace(reg.NodeToken) != "" {
			c.creds.NodeToken = reg.NodeToken
		}
		c.dataMu.Lock()
		c.sessionID = reg.SessionID
		c.dataMu.Unlock()
		if c.onReg != nil {
			if err := c.onReg(reg.NodeUUID, strings.TrimSpace(reg.NodeToken)); err != nil {
				return err
			}
		}
		c.log.Info("registered with server")
		// Then read the server hello.
		endWelcome := c.smokeStage("server_hello")
		if err := c.dec.Decode(&env); err != nil {
			endWelcome()
			return err
		}
		endWelcome()
	}
	if env.T != proto.THello {
		return errors.New("handshake: expected hello, got " + env.T)
	}
	return nil
}
