package placement

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// NodeLimit is a launch-wide node count. Zero leaves the count unrestricted.
type NodeLimit int

func (limit *NodeLimit) UnmarshalYAML(node *yaml.Node) error {
	// yaml.v3 otherwise truncates a floating-point scalar when decoding an int.
	if node.Tag != "!!int" {
		return fmt.Errorf("line %d: placement.max_nodes must be an integer", node.Line)
	}
	var count int
	if err := node.Decode(&count); err != nil {
		return err
	}
	*limit = NodeLimit(count)
	return nil
}
