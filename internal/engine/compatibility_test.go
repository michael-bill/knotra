package engine

import (
	"testing"

	"github.com/michael-bill/knotra/internal/execution/executiontest"
)

func TestTemporalBackendNeutralCompatibility(t *testing.T) {
	executiontest.Run(t, func(t *testing.T, scenario executiontest.Scenario) executiontest.Observation {
		h := newHarness(t)
		h.env.SetStartTime(scenario.Start)
		h.leaf = scenario.Execute
		for _, action := range scenario.Actions {
			h.env.RegisterDelayedCallback(func() {
				switch {
				case action.Human != nil:
					h.env.SignalWorkflow(HumanSignalName, *action.Human)
				case action.Resolution != nil:
					h.env.SignalWorkflow(ResolveSignalName, *action.Resolution)
				case action.Cancel != nil:
					h.env.SignalWorkflow(CancelSignalName, *action.Cancel)
				}
			}, action.At)
		}
		result := h.run(t, scenario.Plan, scenario.Inputs)
		observation := executiontest.Observation{Result: result, Calls: h.calls, Requests: h.requests, TerminalNodes: map[string]string{}}
		for _, event := range h.events {
			if event.Kind == "node" && terminal(event.Status) {
				observation.TerminalNodes[event.InstanceID] = event.Status
			}
		}
		return observation
	})
}
