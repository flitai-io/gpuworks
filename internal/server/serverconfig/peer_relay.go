package serverconfig

import (
	"fmt"
	"math"
	"time"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

// PeerRelay contains this machine's relay resource limits. A zero or omitted
// value retains the shared safe default; it never disables rate limiting.
type PeerRelay struct {
	RateLimitKiBPerSecond int64  `yaml:"rate_limit_kib_s,omitempty"`
	BurstKiB              int64  `yaml:"burst_kib,omitempty"`
	NoProgressTimeout     string `yaml:"no_progress_timeout,omitempty"`
	ConfirmationTimeout   string `yaml:"confirmation_timeout,omitempty"`
}

// UnmarshalYAML keeps integer budgets strict. yaml.v3 otherwise truncates
// floating-point scalars assigned to integers, including 0.5 becoming zero.
func (c *PeerRelay) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!null" {
		*c = PeerRelay{}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("peer_relay must be a mapping")
	}
	seen := make(map[string]bool, 4)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		if seen[key] {
			return fmt.Errorf("peer_relay.%s appears more than once", key)
		}
		seen[key] = true
		switch key {
		case "rate_limit_kib_s", "burst_kib":
			for value.Kind == yaml.AliasNode {
				value = value.Alias
			}
			if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
				return fmt.Errorf("peer_relay.%s must be an integer KiB value", key)
			}
		case "no_progress_timeout", "confirmation_timeout":
		default:
			return fmt.Errorf("field %s not found in peer_relay", key)
		}
	}
	type plain PeerRelay
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = PeerRelay(decoded)
	return nil
}

// Policy resolves and validates the raw host settings for a hub. Callers that
// construct Config directly use the same validation as server.yaml readers.
func (c PeerRelay) Policy() (prepobs.PeerRelayPolicy, error) {
	p := prepobs.DefaultPeerRelayPolicy()
	bytes := func(name string, value int64, fallback int64) (int64, error) {
		if value < 0 || value > math.MaxInt64/1024 {
			return 0, fmt.Errorf("peer_relay.%s must be non-negative and fit in bytes", name)
		}
		if value == 0 {
			return fallback, nil
		}
		return value * 1024, nil
	}
	var err error
	if p.RateLimitBytesPerSecond, err = bytes("rate_limit_kib_s", c.RateLimitKiBPerSecond, p.RateLimitBytesPerSecond); err != nil {
		return prepobs.PeerRelayPolicy{}, err
	}
	if p.BurstBytes, err = bytes("burst_kib", c.BurstKiB, p.BurstBytes); err != nil {
		return prepobs.PeerRelayPolicy{}, err
	}
	duration := func(name, value string, fallback time.Duration) (time.Duration, error) {
		if value == "" {
			return fallback, nil
		}
		d, err := time.ParseDuration(value)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("peer_relay.%s must be a non-negative Go duration", name)
		}
		if d == 0 {
			return fallback, nil
		}
		return d, nil
	}
	if p.NoProgressTimeout, err = duration("no_progress_timeout", c.NoProgressTimeout, p.NoProgressTimeout); err != nil {
		return prepobs.PeerRelayPolicy{}, err
	}
	if p.ConfirmationTimeout, err = duration("confirmation_timeout", c.ConfirmationTimeout, p.ConfirmationTimeout); err != nil {
		return prepobs.PeerRelayPolicy{}, err
	}
	// Leave room for both directions of every active relay. A blocked write
	// must not reserve the whole burst and prevent another relay progressing.
	if maximum := p.BurstBytes / int64(2*p.MaxActive); p.QuantumBytes > maximum {
		p.QuantumBytes = maximum
	}
	if p.QuantumBytes < 1 {
		return prepobs.PeerRelayPolicy{}, fmt.Errorf("peer_relay.burst_kib is too small for the relay concurrency")
	}
	return p, nil
}
