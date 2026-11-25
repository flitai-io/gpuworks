package accelerator

import (
	"strconv"
	"strings"
)

// CardNetwork describes accelerators whose cards carry network ports of
// their own, which the vendor's collective library drives and the host
// kernel never sees. It is the one place that knows which vendors have such
// ports and what a process needs to use them; network resolution, placement
// checks, launch assembly and the agent's probes all read it.
type CardNetwork struct {
	// RailPrefix names a card's port: the prefix followed by the card index
	// ("npu3").
	RailPrefix string
	// AddressEnv and InterfaceEnv are the environment variables that tell
	// the collective library which host address and interface to build its
	// communicator over. The handshake rides the host; only data moves on
	// the cards' ports.
	AddressEnv   []string
	InterfaceEnv []string
	// HostFiles are host files a container needs read-only to find the port
	// of each card it was given.
	HostFiles []string
}

// Ascend 910 cards each own a RoCE port, configured by hccn_tool and recorded
// in /etc/hccn.conf. HCCL moves data between hosts on those ports only; it
// has no switch that sends it over a host interface.
var ascendCardNetwork = CardNetwork{
	RailPrefix:   "npu",
	AddressEnv:   []string{"HCCL_IF_IP"},
	InterfaceEnv: []string{"HCCL_SOCKET_IFNAME"},
	HostFiles:    []string{"/etc/hccn.conf"},
}

// CardNetworkOf reports the card network of a vendor's accelerators, false
// for vendors whose cards have no ports of their own.
func CardNetworkOf(vendor string) (CardNetwork, bool) {
	spec, _ := vendorSpecOf(vendor)
	if spec.cardNetwork == nil {
		return CardNetwork{}, false
	}
	return *spec.cardNetwork, true
}

// PortRailName names the network port an accelerator card owns, as a rail of
// an accelerator RDMA network: "npu3" for Ascend card 3. It is empty for
// vendors whose cards have no port of their own.
func PortRailName(vendor string, index int) string {
	cards, ok := CardNetworkOf(vendor)
	if !ok {
		return ""
	}
	return cards.RailPrefix + strconv.Itoa(index)
}

// PortRailIndex is the card index a rail name stands for, the inverse of
// PortRailName; false when the name is not one of the vendor's.
func PortRailIndex(vendor, name string) (int, bool) {
	cards, ok := CardNetworkOf(vendor)
	if !ok || !strings.HasPrefix(name, cards.RailPrefix) {
		return 0, false
	}
	digits := name[len(cards.RailPrefix):]
	index, err := strconv.Atoi(digits)
	if err != nil || index < 0 || strconv.Itoa(index) != digits {
		return 0, false
	}
	return index, true
}
