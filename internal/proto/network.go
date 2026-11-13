package proto

import (
	"fmt"
	"strconv"
	"strings"
)

// NetworkConfig is the effective per-node network/acceleration config (global
// preset merged with the node override). Two mechanisms (plan §9): URL rewrites
// (xget/1ms/xuanyuan style) and environment (HTTP proxy). DNS acceleration (e.g.
// UAAA 100.90.90.90) is detected/advised, not enforced by the agent.
type NetworkConfig struct {
	HTTPProxy  string `json:"http_proxy,omitempty"`
	HTTPSProxy string `json:"https_proxy,omitempty"`
	// NoProxy lists the hosts reached without the proxy, in the NO_PROXY
	// syntax. The agent adds loopback and the server's own host.
	NoProxy string `json:"no_proxy,omitempty"`
	// Rewrites redirect download URLs (model repositories, archive URLs).
	// Image references are not rewritten here: where an image is pulled from
	// is planned by the server from the pull source rules.
	Rewrites []URLRewrite `json:"rewrites,omitempty"`
	// ServingIP is the node's address on its serving network; the agent runs
	// the serving forwarder there. Empty means no serving network.
	ServingIP string `json:"serving_ip,omitempty"`
	// EnginePorts is the host-port range the server allocates engine ports
	// from. The agent reports its listeners inside it (NetInventory's
	// EnginePortListeners); nil, as from an older server, reports none.
	EnginePorts *PortRange `json:"engine_ports,omitempty"`
}

// PortRange is an inclusive range of TCP ports.
type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Valid reports whether the range names at least one usable TCP port.
func (r PortRange) Valid() bool { return r.Start >= 1 && r.Start <= r.End && r.End <= 65535 }

// ParsePortRange reads "START-END", the form --engine-ports takes.
func ParsePortRange(s string) (PortRange, error) {
	lo, hi, ok := strings.Cut(strings.TrimSpace(s), "-")
	start, err1 := strconv.Atoi(strings.TrimSpace(lo))
	end, err2 := strconv.Atoi(strings.TrimSpace(hi))
	r := PortRange{Start: start, End: end}
	if !ok || err1 != nil || err2 != nil || !r.Valid() {
		return PortRange{}, fmt.Errorf("port range %q: want START-END within 1-65535", s)
	}
	return r, nil
}

// Contains reports whether port lies in the range.
func (r PortRange) Contains(port int) bool { return r.Start <= port && port <= r.End }

// URLRewrite rewrites a URL whose start matches From by replacing that prefix
// with To (applied to downloads and image pulls).
type URLRewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Apply rewrites url if any rule matches (longest From wins).
func (n NetworkConfig) Apply(url string) string {
	best := ""
	var bestTo string
	for _, rw := range n.Rewrites {
		if len(rw.From) > len(best) && len(url) >= len(rw.From) && url[:len(rw.From)] == rw.From {
			best = rw.From
			bestTo = rw.To
		}
	}
	if best != "" {
		return bestTo + url[len(best):]
	}
	return url
}
