//go:build darwin

package collector

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// hostSampler on macOS is the macSampler wired to the real sources.
type hostSampler = macSampler

func newHostSampler() *hostSampler {
	cpu := &iostatCPU{}
	return &macSampler{cpuBusy: cpu.latest, disks: ioregDisks, interfaces: routeInterfaces}
}

// iostatCPU keeps one `iostat -C` running and remembers its latest line. A
// process per tick would cost more than the reading is worth, and macOS offers
// no cgo-free call for CPU ticks. If iostat exits it is restarted on a later
// reading, at most once a minute. When the agent exits, iostat dies on its
// next write to the closed pipe.
type iostatCPU struct {
	mu        sync.Mutex
	busy      float64
	at        time.Time
	have      bool
	running   bool
	lastStart time.Time
}

func (c *iostatCPU) latest() (float64, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running && time.Since(c.lastStart) >= time.Minute {
		c.running, c.lastStart = true, time.Now()
		go c.run()
	}
	return c.busy, c.at, c.have
}

func (c *iostatCPU) run() {
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
	cmd := exec.Command("iostat", "-n", "0", "-C", "-w", strconv.Itoa(int(macIostatInterval/time.Second)))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		slog.Warn("host cpu sampling unavailable", "err", err)
		return
	}
	defer func() { _ = cmd.Wait() }()
	sc := bufio.NewScanner(stdout)
	first := true
	for sc.Scan() {
		busy, ok := parseIostatCPU(sc.Text())
		if !ok {
			continue
		}
		// The first report averages since boot, not over an interval.
		if first {
			first = false
			continue
		}
		c.mu.Lock()
		c.busy, c.at, c.have = busy, time.Now(), true
		c.mu.Unlock()
	}
}

// ioregDisks reads every physical disk's cumulative counters.
func ioregDisks() (map[string]macDiskIO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ioreg", "-r", "-c", "IOBlockStorageDriver", "-w", "0", "-d", "2", "-l").Output()
	if err != nil {
		return nil, err
	}
	return parseBlockStorage(string(out)), nil
}

// routeInterfaces reads every Ethernet and Wi-Fi port's counters from the
// routing sysctl (64-bit counters, no process).
func routeInterfaces() (map[string]macIfCounters, error) {
	buf, err := route.FetchRIB(unix.AF_UNSPEC, unix.NET_RT_IFLIST2, 0)
	if err != nil {
		return nil, err
	}
	out := map[string]macIfCounters{}
	for index, counters := range parseIfList2(buf) {
		ifc, err := net.InterfaceByIndex(int(index))
		if err != nil || !isMacTrafficInterface(ifc.Name) {
			continue
		}
		out[ifc.Name] = counters
	}
	return out, nil
}
