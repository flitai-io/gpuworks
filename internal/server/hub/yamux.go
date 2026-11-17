package hub

import (
	"io"
	"log/slog"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Lane yamux tuning. The values come from the adaptive-bandwidth tunnel plan
// (artifacts/archive/2026-09-08-tunnel-bandwidth/plan.md §3.1).
//
// The data lane uses go-yamux's receive-window auto-tuning: it starts at 256
// KiB and grows toward 16 MiB as the measured RTT allows, so a single stream
// can fill a high bandwidth-delay-product link instead of being capped at
// window/RTT (~87 Mbps at 20 ms with a fixed 256 KiB window). The ctrl lane
// pins the window at 256 KiB because it carries only small, latency-sensitive
// control traffic and never needs a large window.
//
// The keepalive interval must stay well short of half the connection write
// timeout. go-yamux re-arms the write deadline only once half the timeout has
// elapsed since the last re-arm, so on a lane with nothing else to say the
// keepalive ping is the write that has to do it. With the two equal (15 s and
// 30 s) that ping lands within a millisecond of the deadline on every cycle,
// and the two ends' keepalive timers start together, so their pings and pongs
// drift past the instant together: the frame then races the deadline callback
// inside coder/websocket's net.Conn, and a callback that catches the frame
// mid-write cancels the write context for good and closes the whole
// connection. That dropped every agent lane about four times an hour, and
// every web terminal with it (they ride the ctrl lane). At 10 s the ping
// always re-arms with at least 5 s to spare. TestLaneYamuxConfig_MatchesPlan
// guards the margin.
const (
	ctrlLaneWindow        = 256 * 1024       // Initial == Max: fixed small window.
	dataLaneInitialWindow = 256 * 1024       // Auto-tuning starts here.
	dataLaneMaxWindow     = 16 * 1024 * 1024 // Auto-tuning ceiling.
	laneKeepAliveInterval = 10 * time.Second // Independent ping channel in go-yamux.
	laneConnWriteTimeout  = 30 * time.Second // "Safety valve" for a wedged write.
	ctrlLaneMaxStreams    = 256              // ctrl/terminal only.
	dataLaneMaxStreams    = 4096             // tunnel/logtail/model/image fan-out.
)

// laneYamuxConfig builds the go-yamux configuration for a lane. A single-lane
// agent gets one session with the data-lane settings; a dual-lane agent also
// gets one built with proto.LaneCtrl for the dedicated control connection, so
// the lane parameter already selects the right window and stream-count
// profile.
func laneYamuxConfig(lane string, logOut io.Writer) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = laneKeepAliveInterval
	cfg.ConnectionWriteTimeout = laneConnWriteTimeout
	if logOut != nil {
		cfg.LogOutput = logOut
	}
	switch lane {
	case proto.LaneCtrl:
		cfg.InitialStreamWindowSize = ctrlLaneWindow
		cfg.MaxStreamWindowSize = ctrlLaneWindow
		cfg.MaxIncomingStreams = ctrlLaneMaxStreams
	default: // proto.LaneData
		cfg.InitialStreamWindowSize = dataLaneInitialWindow
		cfg.MaxStreamWindowSize = dataLaneMaxWindow
		cfg.MaxIncomingStreams = dataLaneMaxStreams
	}
	return cfg
}

// slogWriter adapts yamux's io.Writer logging into slog at debug level.
type slogWriter struct{ log *slog.Logger }

func (w *slogWriter) Write(p []byte) (int, error) {
	w.log.Debug("yamux", "msg", string(p))
	return len(p), nil
}
