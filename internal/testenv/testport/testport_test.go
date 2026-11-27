package testport

import (
	"net"
	"strconv"
	"testing"
)

// restart makes the next handout begin a new run, as a new process would.
func restart() {
	mu.Lock()
	next = -1
	mu.Unlock()
}

// upcoming is the port the next handout tries first.
func upcoming(t *testing.T) int {
	t.Helper()
	Port(t, "127.0.0.1") // a run has begun
	mu.Lock()
	defer mu.Unlock()
	return poolStart + next
}

func listen(t *testing.T, port int) (net.Listener, error) {
	t.Helper()
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// Ports come from the pool, are handed out once, leave the port after each
// unused, can be bound afterwards together with that port, and form one run.
func TestPortsAreInThePoolDistinctSpacedAndBindable(t *testing.T) {
	restart()
	end := poolEnd()
	seen := map[int]bool{}
	first := 0
	for i := range 50 {
		port := Port(t, "127.0.0.1")
		if i == 0 {
			first = port
		}
		if port < poolStart || port+1 >= end {
			t.Fatalf("port %d or the one after it is outside the pool %d-%d", port, poolStart, end-1)
		}
		if seen[port] || seen[port-1] || seen[port+1] {
			t.Fatalf("port %d was handed out twice or is next to one handed out", port)
		}
		seen[port] = true
		for _, bound := range []int{port, port + 1} {
			listener, err := listen(t, bound)
			if err != nil {
				t.Fatalf("port %d, handed out with %d, cannot be bound: %v", bound, port, err)
			}
			defer listener.Close()
		}
	}
	// One process uses one short run of ports, so that processes that start
	// at different places stay apart. Pairs already in use are skipped, so
	// the run can be longer than the hundred ports handed out.
	size := end - poolStart
	for port := range seen {
		if distance := (port - first + size) % size; distance > 600 {
			t.Fatalf("port %d is %d ports after the first, %d: not one run", port, distance, first)
		}
	}
}

// A pair is skipped when either of its ports is in use: the next handout is a
// later pair of the same run.
func TestPortSkipsAPairWithAPortInUse(t *testing.T) {
	for _, c := range []struct {
		name   string
		offset int
	}{{"the port itself", 0}, {"the port after it", 1}} {
		t.Run(c.name, func(t *testing.T) {
			var held net.Listener
			var pair int
			// A pair that something else already uses is stepped over.
			for range 200 {
				pair = upcoming(t)
				listener, err := listen(t, pair+c.offset)
				if err == nil {
					held = listener
					break
				}
			}
			if held == nil {
				t.Fatal("no upcoming port could be bound in 200 pairs")
			}
			defer held.Close()
			got := Port(t, "127.0.0.1")
			size := poolEnd() - poolStart
			if distance := (got - pair + size) % size; distance < 2 || distance > 600 {
				t.Fatalf("with %d in use, the handout after pair %d was %d", pair+c.offset, pair, got)
			}
		})
	}
}

// Each process starts its run at a random place, which is what keeps
// processes that know nothing of each other apart.
func TestARunStartsAtARandomPlace(t *testing.T) {
	lowest, highest := poolEndMost, 0
	for range 8 {
		restart()
		start := Port(t, "127.0.0.1")
		lowest, highest = min(lowest, start), max(highest, start)
	}
	// A fixed start would put the eight runs side by side, a few ports
	// apart. Eight random starts all fall within a twentieth of the pool
	// less than once in a hundred million runs, whatever the pool's size.
	if spread := (poolEnd() - poolStart) / 20; highest-lowest < spread {
		t.Fatalf("8 runs started between %d and %d: the start is not random", lowest, highest)
	}
}
