package gpu

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Each supported Ascend card exposes accelerator fabric ports the host kernel
// does not see; hccn_tool is the only way to read them. A query takes about
// 0.3 s and one on a card the
// host does not own (passed through to a VM) never returns, so every call is
// time-limited and the result is cached: addresses and link state change on
// the scale of recabling, not of status reports.
const (
	ascendPortsTTL     = time.Minute
	ascendPortsTimeout = 3 * time.Second
)

var (
	_ AcceleratorPortProber         = (*AscendProber)(nil)
	_ AcceleratorPortPinger         = (*AscendProber)(nil)
	_ AcceleratorPortProtocolPinger = (*AscendProber)(nil)
)

// ascendCardPorts is what the prober keeps about the cards' ports: the last
// inventory it read, and one lock per card for the vendor tool.
type ascendCardPorts struct {
	mu    sync.Mutex
	ports []proto.AcceleratorPort
	at    time.Time

	// cards holds one lock per card: two pings on one card at once leave
	// the card answering "Command execute failed!" for a while (measured on
	// 910B4), so every hccn_tool call on a card runs in turn, whoever asks.
	cardMu sync.Mutex
	cards  map[int]*sync.Mutex
	// owned is the cards of the last inventory, nil until one was read. A
	// ping checks a card against it without waiting for a refresh, which
	// itself waits for the cards' locks.
	owned map[int]bool
}

// AcceleratorPorts implements AcceleratorPortProber: the accelerator ports
// of every card npu-smi lists, ordered by card index. A card whose port
// answers nothing is left out; a host without hccn_tool has none.
func (p *AscendProber) AcceleratorPorts(ctx context.Context) []proto.AcceleratorPort {
	p.ports.mu.Lock()
	defer p.ports.mu.Unlock()
	ttl := p.portsTTL
	if ttl <= 0 {
		ttl = ascendPortsTTL
	}
	if !p.ports.at.IsZero() && time.Since(p.ports.at) < ttl {
		return append([]proto.AcceleratorPort(nil), p.ports.ports...)
	}
	gpus, err := p.Probe(ctx)
	if err != nil {
		return nil
	}
	run := p.hccn
	if run == nil {
		if _, err := lookCLI("hccn_tool"); err != nil {
			return nil
		}
		run = runHCCN
	}
	// Cards are queried side by side, the three questions of one card in
	// turn: concurrent hccn_tool calls on one card are not reliable.
	found := make([]*proto.AcceleratorPort, len(gpus))
	var wg sync.WaitGroup
	for i, g := range gpus {
		if g.Index < 0 || !accelerator.CardUsable(g.Ownership) {
			// No device to ask about: a card the CLI did not list has no
			// hccn index, and one lent elsewhere is not ours to query.
			// Asking anyway sent every one of them to `hccn_tool -i -1`,
			// all serialized behind the same lock, each with its own
			// timeout — sixteen pointless calls per refresh on a host
			// with four passed-through cards.
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			card := p.cardLock(g.Index)
			card.Lock()
			defer card.Unlock()
			found[i] = ascendPort(ctx, run, g.Index)
		}()
	}
	wg.Wait()
	ports := []proto.AcceleratorPort{}
	for _, port := range found {
		if port != nil {
			ports = append(ports, *port)
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Index < ports[j].Index })
	p.ports.ports, p.ports.at = ports, time.Now()
	owned := map[int]bool{}
	for _, port := range ports {
		owned[port.Index] = true
	}
	p.ports.cardMu.Lock()
	p.ports.owned = owned
	p.ports.cardMu.Unlock()
	return append([]proto.AcceleratorPort(nil), ports...)
}

func ascendPort(ctx context.Context, run ascendCommandRunner, index int) *proto.AcceleratorPort {
	ask := func(what string) string {
		out, err := run(ctx, "-i", strconv.Itoa(index), what, "-g")
		if err != nil {
			return ""
		}
		return out
	}
	port := proto.AcceleratorPort{Name: accelerator.PortRailName(accelerator.VendorAscend, index), Index: index}
	port.IP, port.Netmask = parseHCCNAddress(ask("-ip"))
	port.State = parseHCCNLink(ask("-link"))
	vnic := ask("-vnic")
	port.HCCSIP, port.HCCSNetmask = parseHCCNVNICAddress(vnic)
	port.HCCSState = parseHCCNVNICLink(vnic)
	if port.IP == "" && port.State == "" && port.HCCSIP == "" && port.HCCSState == "" {
		return nil
	}
	port.SpeedGbps = parseHCCNSpeed(ask("-speed"))
	return &port
}

// runHCCN asks hccn_tool about one die, at most ascendPortsTimeout, under
// that die's own key (RunTracked). A call stuck in the driver holds only its
// die: that die's later calls fail at once instead of piling up behind it,
// and the status report that reads the ports is never held past the bound.
func runHCCN(ctx context.Context, args ...string) (string, error) {
	return RunTracked(ctx, hccnKey(args), ascendPortsTimeout, func(ctx context.Context) *exec.Cmd {
		return cliCommand(ctx, "hccn_tool", args...)
	})
}

// hccnKey is the die a hccn_tool call addresses ("-i <die>").
func hccnKey(args []string) string {
	if len(args) >= 2 && args[0] == "-i" {
		return "hccn_tool -i " + args[1]
	}
	return "hccn_tool"
}

var (
	hccnIPRE       = regexp.MustCompile(`(?m)^ipaddr:\s*(\S+)`)
	hccnNetmaskRE  = regexp.MustCompile(`(?m)^netmask:\s*(\S+)`)
	hccnLinkRE     = regexp.MustCompile(`(?mi)^link status:\s*(UP|DOWN)\b`)
	hccnVNICIPRE   = regexp.MustCompile(`(?m)^vnic ipaddr:\s*(\S+)`)
	hccnVNICMaskRE = regexp.MustCompile(`(?m)^vnic netmask:\s*(\S+)`)
	hccnVNICLinkRE = regexp.MustCompile(`(?mi)^vnic link status:\s*(UP|DOWN)\b`)
	hccnSpeedRE    = regexp.MustCompile(`(?mi)^Speed:\s*([0-9]+)\s*Mb/s`)
)

// parseHCCNAddress reads "ipaddr:203.0.113.59\nnetmask:255.255.0.0".
func parseHCCNAddress(out string) (ip, netmask string) {
	if m := hccnIPRE.FindStringSubmatch(out); m != nil {
		ip = m[1]
	}
	if m := hccnNetmaskRE.FindStringSubmatch(out); m != nil {
		netmask = m[1]
	}
	return ip, netmask
}

func parseHCCNVNICAddress(out string) (ip, netmask string) {
	if m := hccnVNICIPRE.FindStringSubmatch(out); m != nil {
		ip = m[1]
	}
	if m := hccnVNICMaskRE.FindStringSubmatch(out); m != nil {
		netmask = m[1]
	}
	return ip, netmask
}

// parseHCCNLink reads "link status: UP"; anything else is unknown.
func parseHCCNLink(out string) string {
	if m := hccnLinkRE.FindStringSubmatch(out); m != nil {
		return strings.ToUpper(m[1])
	}
	return ""
}

func parseHCCNVNICLink(out string) string {
	if m := hccnVNICLinkRE.FindStringSubmatch(out); m != nil {
		return strings.ToUpper(m[1])
	}
	return ""
}

// parseHCCNSpeed reads "Speed: 200000 Mb/s"; a port that is down answers
// "Speed: Unknown!", which is 0.
func parseHCCNSpeed(out string) float64 {
	if m := hccnSpeedRE.FindStringSubmatch(out); m != nil {
		if mbps, err := strconv.ParseFloat(m[1], 64); err == nil {
			return mbps / 1000
		}
	}
	return 0
}

// PingAcceleratorPort implements AcceleratorPortPinger with the legacy RoCE
// command "hccn_tool -i N -ping -g address IP".
func (p *AscendProber) PingAcceleratorPort(ctx context.Context, card, target string, timeout time.Duration) (float64, error) {
	return p.PingAcceleratorPortProtocol(ctx, card, target, proto.AcceleratorPingProtocolRoCE, timeout)
}

// PingAcceleratorPortProtocol pings through the requested accelerator fabric.
// RoCE uses hccn_tool -ping and HCCS uses hccn_tool -hccs_ping. Both commands
// send three packets and
// prints everything at once when done, about a second later; it has no
// timeout of its own and never returns when the address does not answer, so
// the limit is enforced here by ending the process.
func (p *AscendProber) PingAcceleratorPortProtocol(ctx context.Context, card, target, protocol string, timeout time.Duration) (float64, error) {
	index, ok := accelerator.PortRailIndex(accelerator.VendorAscend, card)
	if !ok || !p.ownsCard(index) {
		return 0, ErrAcceleratorNoCard
	}
	ip := net.ParseIP(target)
	if ip == nil || ip.To4() == nil {
		return 0, fmt.Errorf("ping target %q is not an IPv4 address", target)
	}
	run := p.hccn
	if run == nil {
		if _, err := lookCLI("hccn_tool"); err != nil {
			return 0, errors.New("hccn_tool is not installed")
		}
		run = runHCCNUnlimited
	}
	lock := p.cardLock(index)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := "-ping"
	if protocol == proto.AcceleratorPingProtocolHCCS {
		command = "-hccs_ping"
	}
	out, err := run(ctx, "-i", strconv.Itoa(index), command, "-g", "address", ip.String())
	if rtt, ok := parseHCCNPing(out); ok {
		return rtt, nil
	}
	if ctx.Err() != nil {
		return 0, ErrAcceleratorPingTimeout
	}
	if detail := strings.TrimSpace(out); detail != "" {
		return 0, errors.New(lastLine(detail))
	}
	if err != nil {
		return 0, err
	}
	return 0, errors.New("hccn_tool answered nothing")
}

// ownsCard checks a card against the last port inventory. Before any
// inventory was read nothing is known, and the vendor tool is left to say.
func (p *AscendProber) ownsCard(index int) bool {
	p.ports.cardMu.Lock()
	defer p.ports.cardMu.Unlock()
	return p.ports.owned == nil || p.ports.owned[index]
}

func (p *AscendProber) cardLock(index int) *sync.Mutex {
	p.ports.cardMu.Lock()
	defer p.ports.cardMu.Unlock()
	if p.ports.cards == nil {
		p.ports.cards = map[int]*sync.Mutex{}
	}
	if p.ports.cards[index] == nil {
		p.ports.cards[index] = &sync.Mutex{}
	}
	return p.ports.cards[index]
}

// runHCCNUnlimited runs hccn_tool until the caller's context ends it.
// It shares the die's key with runHCCN, so a ping stuck in the driver is not
// joined by another.
func runHCCNUnlimited(ctx context.Context, args ...string) (string, error) {
	return RunTracked(ctx, hccnKey(args), 0, func(ctx context.Context) *exec.Cmd {
		return cliCommand(ctx, "hccn_tool", args...)
	})
}

var (
	hccnPingSummaryRE = regexp.MustCompile(`(?m)^(\d+) packets transmitted, (\d+) received`)
	hccnPingRecvRE    = regexp.MustCompile(`(?m)^recv seq=\d+,time=([0-9.]+)ms`)
	hccsPingRecvRE    = regexp.MustCompile(`(?m)^recv seq\s+\d+,\s*time\s*=\s*([0-9.]+)us`)
)

// parseHCCNPing reads
//
//	device 3 PING 203.0.113.82
//	recv seq=0,time=0.359000ms
//	3 packets transmitted, 3 received, 0.00% packet loss
//
// and reports the mean round trip of the packets that came back. One answer
// is enough to call the address reachable.
func parseHCCNPing(out string) (rttMS float64, ok bool) {
	summary := hccnPingSummaryRE.FindStringSubmatch(out)
	if summary == nil {
		return 0, false
	}
	if received, err := strconv.Atoi(summary[2]); err != nil || received == 0 {
		return 0, false
	}
	var sum float64
	var n int
	for _, m := range hccnPingRecvRE.FindAllStringSubmatch(out, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			sum += v
			n++
		}
	}
	for _, m := range hccsPingRecvRE.FindAllStringSubmatch(out, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			sum += v / 1000
			n++
		}
	}
	if n == 0 {
		return 0, true
	}
	return sum / float64(n), true
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
