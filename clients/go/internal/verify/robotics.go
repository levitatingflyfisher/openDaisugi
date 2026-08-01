package verify

import "strconv"

// CheckPlanInvariantsRobotics ports z3_checks.check_plan_invariants: the
// dedicated numeric/geometric handlers for the four RECOGNIZED_OPAQUE_TYPES
// robotics invariants. Deterministic float64 math — no solver needed (the
// Python oracle doesn't use Z3 for these either; the "z3" stage name is
// historical/organizational, not literal).
func CheckPlanInvariantsRobotics(plan ActionPlan, env Envelope) []Violation {
	declared := map[string]bool{}
	for _, inv := range env.Invariants {
		declared[inv.Type] = true
	}
	var violations []Violation
	if declared["end_effector_in_workspace"] {
		violations = append(violations, checkWorkspaceContainment(plan, env)...)
	}
	if declared["joint_limits_respected"] {
		violations = append(violations, checkJointLimits(plan, env)...)
	}
	if declared["velocity_bounded"] {
		violations = append(violations, checkVelocityBounds(plan, env)...)
	}
	if declared["no_obstacle_penetration"] {
		violations = append(violations, checkObstacleAvoidance(plan, env)...)
	}
	return violations
}

func checkWorkspaceContainment(plan ActionPlan, env Envelope) []Violation {
	bounds := env.Permissions.WorkspaceBounds
	if bounds == nil {
		return nil
	}
	lo, hi := bounds[0], bounds[1]
	var violations []Violation
	for _, step := range plan.Steps {
		var target [3]float64
		var ok bool
		switch step.Type {
		case "cartesian_move":
			target, ok = step.TargetPosition()
		case "vla":
			target, ok = step.TargetPose()
		}
		if !ok {
			continue
		}
		inBounds := true
		for i := 0; i < 3; i++ {
			if target[i] < lo[i] || target[i] > hi[i] {
				inBounds = false
				break
			}
		}
		if !inBounds {
			violations = append(violations, VStep("z3", step.ID))
		}
	}
	return violations
}

func checkJointLimits(plan ActionPlan, env Envelope) []Violation {
	limits := env.Permissions.JointLimits
	if len(limits) == 0 {
		return nil
	}
	var violations []Violation
	for _, step := range plan.Steps {
		if step.Type != "joint_move" {
			continue
		}
		targets := step.JointTargets()
		for joint, target := range targets {
			rng, ok := limits[joint]
			if !ok {
				violations = append(violations, VStep("z3", step.ID))
				continue
			}
			if target < rng[0] || target > rng[1] {
				violations = append(violations, VStep("z3", step.ID))
			}
		}
	}
	return violations
}

func checkVelocityBounds(plan ActionPlan, env Envelope) []Violation {
	limit := env.Permissions.VelocityLimit
	if limit == nil {
		return nil
	}
	state := map[string]float64{}
	var violations []Violation
	for _, step := range plan.Steps {
		if step.Type != "joint_move" {
			continue
		}
		targets := step.JointTargets()
		duration := step.DurationS()
		if duration < 1e-6 {
			duration = 1e-6
		}
		for joint, target := range targets {
			prev := state[joint]
			delta := target - prev
			if delta < 0 {
				delta = -delta
			}
			peak := delta / duration * step.VelocityScale()
			if peak > *limit {
				violations = append(violations, VStep("z3", step.ID))
			}
			state[joint] = target
		}
	}
	return violations
}

const obstacleMidpointSamples = 8

func interpolatePositions(p0, p1 [3]float64, n int) [][3]float64 {
	out := make([][3]float64, 0, n)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(n-1)
		out = append(out, [3]float64{
			p0[0] + (p1[0]-p0[0])*t,
			p0[1] + (p1[1]-p0[1])*t,
			p0[2] + (p1[2]-p0[2])*t,
		})
	}
	return out
}

func checkObstacleAvoidance(plan ActionPlan, env Envelope) []Violation {
	obstacles := env.Permissions.Obstacles
	if len(obstacles) == 0 {
		return nil
	}
	type sample struct {
		stepID string
		pt     [3]float64
	}
	var samples []sample
	prev := [3]float64{0, 0, 0}
	any := false
	for _, step := range plan.Steps {
		if step.Type != "cartesian_move" {
			continue
		}
		target, ok := step.TargetPosition()
		if !ok {
			continue
		}
		any = true
		for _, pt := range interpolatePositions(prev, target, obstacleMidpointSamples) {
			samples = append(samples, sample{stepID: step.ID, pt: pt})
		}
		prev = target
	}
	if !any {
		return nil
	}
	var violations []Violation
	flaggedKeys := map[string]bool{}
	for _, s := range samples {
		for idx, obstacle := range obstacles {
			key := s.stepID + "\x00" + strconv.Itoa(idx)
			if flaggedKeys[key] {
				continue
			}
			lo, hi := obstacle[0], obstacle[1]
			if s.pt[0] >= lo[0] && s.pt[0] <= hi[0] &&
				s.pt[1] >= lo[1] && s.pt[1] <= hi[1] &&
				s.pt[2] >= lo[2] && s.pt[2] <= hi[2] {
				violations = append(violations, VStep("z3", s.stepID))
				flaggedKeys[key] = true
			}
		}
	}
	return violations
}

