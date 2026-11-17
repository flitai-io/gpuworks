package app

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/logline"
	"gpuworks.ai/gpuworks/internal/logtext"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Node infrastructure logs make "the instance is fine but the node is not"
// searchable next to instance logs. The agent tails journald for the platform
// units at warning priority and ships them under source=node; the server
// stores them in the same partitions but the UI keeps them out of the default
// instance view.

const (
	// nodeLogRatePerMin caps a journald storm: anything beyond this many lines
	// per rolling minute is dropped (and counted) rather than flooding the
	// control plane from every node at once.
	nodeLogRatePerMin = 120
)

// journalStreamer is the single OS touchpoint, injected for tests.
type journalStreamer interface {
	Stream(ctx context.Context, units []string) (io.ReadCloser, error)
}

// journald tails systemd journal units via journalctl. A missing or
// non-functional journalctl (containers, non-systemd nodes) disables the
// follower instead of erroring forever.
type journald struct{}

func (journald) Stream(ctx context.Context, units []string) (io.ReadCloser, error) {
	bin, err := exec.LookPath("journalctl")
	if err != nil {
		return nil, fmt.Errorf("journalctl not available: %w", err)
	}
	args := []string{"-f", "-p", "warning", "--merge"}
	for _, u := range units {
		args = append(args, "-u", u)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	return stdout, nil
}

// nodeLogUnits lists the units whose warnings matter for the node's health,
// extended by the vendor's driver units.
func nodeLogUnits(vendor string) []string {
	units := []string{"docker", "containerd"}
	switch vendor {
	case accelerator.VendorNVIDIA:
		units = append(units, "nvidia-persistenced")
	case accelerator.VendorAscend:
		units = append(units, "ascend-dmi-service")
	}
	return units
}

// shipNodeLogs follows node infrastructure logs for the agent's lifetime. A
// dead or missing journalctl disables the follower quietly — nodes without
// systemd simply have no node logs — while a stream that ends later
// (journal rotation, journald restart) reconnects after a pause.
func (n *NodeRuntime) shipNodeLogs(ctx context.Context) {
	if n.journal == nil {
		return
	}
	units := nodeLogUnits(n.prober.Vendor())
	for {
		rc, err := n.journal.Stream(ctx, units)
		if err == nil {
			n.followNodeLogs(ctx, rc)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// followNodeLogs consumes one journald stream: severity stamps, a rolling
// per-minute rate cap, and node-source batches.
func (n *NodeRuntime) followNodeLogs(ctx context.Context, rc io.ReadCloser) {
	defer rc.Close()
	sc := logtext.NewScanner(rc)

	batch := make([]proto.LogLine, 0, 32)
	var window []time.Time
	admit := func(now time.Time) bool {
		for len(window) > 0 && now.Sub(window[0]) > time.Minute {
			window = window[1:]
		}
		if len(window) >= nodeLogRatePerMin {
			return false
		}
		window = append(window, now)
		return true
	}

	flush := time.NewTicker(2 * time.Second)
	defer flush.Stop()
	lineCh := make(chan string, 128)
	go func() {
		for sc.Scan() {
			select {
			case lineCh <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		close(lineCh)
	}()

	send := func() {
		if len(batch) == 0 {
			return
		}
		n.emit(proto.TLogs, proto.LogBatch{Lines: batch})
		// Hand over ownership: the connection may marshal the batch
		// asynchronously, so the next lines must append into a fresh slice
		// instead of reusing the handed-off backing array.
		batch = make([]proto.LogLine, 0, 32)
	}
	for {
		select {
		case <-ctx.Done():
			send()
			return
		case line, ok := <-lineCh:
			if !ok {
				send()
				return
			}
			if !admit(time.Now()) {
				if dropped := n.logNodeDropped.Add(1); dropped%1000 == 1 {
					n.log.Info("dropping node log lines over the rate cap", "dropped", dropped)
				}
				continue
			}
			// The follower only reads warning-priority journal entries, so a
			// line without its own stamp is at least a warning.
			level := logline.Level(line)
			if level == "" {
				level = logline.LevelWarn
			}
			batch = append(batch, proto.LogLine{
				TimeMS: time.Now().UnixMilli(), Line: line,
				Level: level, Source: "node",
			})
			if len(batch) >= 32 {
				send()
			}
		case <-flush.C:
			send()
		}
	}
}
