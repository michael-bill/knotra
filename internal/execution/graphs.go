package execution

import (
	"fmt"
	"strings"

	"github.com/michael-bill/knotra/internal/contract"
)

// GraphDefinition resolves the existing public graph path against the frozen
// pipeline. Inline bodies keep their containing pipeline's defaults/resources.
func GraphDefinition(plan contract.Plan, pipeline, path string) (contract.Graph, error) {
	document := plan.Pipelines[pipeline]
	if document == nil {
		return contract.Graph{}, fmt.Errorf("pipeline %q is not in the frozen plan", pipeline)
	}
	graph := document.Spec.Graph
	if path == "" {
		return graph, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for len(parts) > 0 {
		if len(parts) < 3 || parts[0] != "nodes" || parts[2] != "body" {
			return contract.Graph{}, fmt.Errorf("invalid graph path %q", path)
		}
		node, found := graph.Nodes[parts[1]]
		if !found {
			return contract.Graph{}, fmt.Errorf("graph path %q is not in the frozen plan", path)
		}
		switch {
		case node.Type == "foreach" && node.Foreach != nil:
			graph = node.Foreach.Body
		case node.Type == "loop" && node.Loop != nil:
			graph = node.Loop.Body
		default:
			return contract.Graph{}, fmt.Errorf("node %q has no inline body", parts[1])
		}
		parts = parts[3:]
	}
	return graph, nil
}
