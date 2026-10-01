package verify

import (
	"encoding/json"
	"testing"

	"daisugi-verify/internal/pyjson"
)

// Each robotics violation carries the oracle's detail dict, so a run the
// robotics checks reject is journaled as the oracle journals it.
func TestRoboticsViolationDetails(t *testing.T) {
	env, err := ParseEnvelope(json.RawMessage(`{"generated_by": "t", "task": "t", "permissions": {
	  "joint_limits": {"j1": [-1, 1]}, "velocity_limit": 0.5, "workspace_bounds": [[-1, -1, -1], [1, 1, 1]],
	  "obstacles": [[[0.1, -0.1, -0.1], [0.3, 0.1, 0.1]]]},
	  "invariants": [{"type": "end_effector_in_workspace", "description": "d"}, {"type": "joint_limits_respected", "description": "d"},
	    {"type": "velocity_bounded", "description": "d"}, {"type": "no_obstacle_penetration", "description": "d"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParsePlan(json.RawMessage(`{"source": "t", "task": "t", "steps": [
	  {"type": "joint_move", "id": "a", "joint_targets": {"j1": 1.5, "j9": 0.2}},
	  {"type": "cartesian_move", "id": "b", "target_position": [0.4, 0, 0]},
	  {"type": "vla", "id": "c", "task": "x", "target_pose": [2, 0, 0]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"invariant": "end_effector_in_workspace", "step": "c", "target": [2.0, 0.0, 0.0], "bounds": [[-1.0, -1.0, -1.0], [1.0, 1.0, 1.0]]}`,
		`{"invariant": "joint_limits_respected", "step": "a", "joint": "j1", "target": 1.5, "range": [-1.0, 1.0]}`,
		`{"invariant": "joint_limits_respected", "step": "a", "joint": "j9"}`,
		`{"invariant": "velocity_bounded", "step": "a", "joint": "j1", "peak_rad_s": 1.5, "limit_rad_s": 0.5}`,
		`{"invariant": "no_obstacle_penetration", "step": "b", "obstacle_index": 0, "sample_point": [0.11428571428571428, 0.0, 0.0]}`,
	}
	got := CheckPlanInvariantsRobotics(plan, env)
	if len(got) != len(want) {
		t.Fatalf("%d violations: %+v", len(got), got)
	}
	for i, v := range got {
		if !v.Known || pyjson.Dumps(v.Detail, true) != want[i] {
			t.Fatalf("violation %d (%s): known %v, detail %s", i, v.Message, v.Known, pyjson.Dumps(v.Detail, true))
		}
	}
}
