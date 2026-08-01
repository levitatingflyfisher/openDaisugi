package verify

import "testing"

func stepRaw(id, typ string, fields map[string]interface{}) Step {
	m := map[string]interface{}{"id": id, "type": typ, "depends_on": []interface{}{}, "metadata": map[string]interface{}{}}
	for k, v := range fields {
		m[k] = v
	}
	var deps []string
	if d, ok := fields["depends_on"].([]interface{}); ok {
		for _, x := range d {
			if s, ok := x.(string); ok {
				deps = append(deps, s)
			}
		}
	}
	return Step{ID: id, Type: typ, DependsOn: deps, Raw: m}
}

func TestCheckPermissions_ShellDenied(t *testing.T) {
	plan := ActionPlan{Steps: []Step{stepRaw("s1", "shell", map[string]interface{}{"command": "git status"})}}
	env := DefaultEnvelope() // Permissions.Shell defaults false
	v := CheckPermissions(plan, env, false)
	if len(v) != 1 || v[0].Stage != "permissions" || v[0].Step != "s1" {
		t.Fatalf("want one permissions violation on s1, got %+v", v)
	}
}

func TestCheckPermissions_ShellAllowlist(t *testing.T) {
	env := DefaultEnvelope()
	env.Permissions.Shell = true
	env.Permissions.ShellAllowlist = []string{"git"}
	plan := ActionPlan{Steps: []Step{
		stepRaw("s1", "shell", map[string]interface{}{"command": "git status"}),
		stepRaw("s2", "shell", map[string]interface{}{"command": "rm -rf /"}),
	}}
	v := CheckPermissions(plan, env, false)
	if len(v) != 1 || v[0].Step != "s2" {
		t.Fatalf("want exactly one violation on s2, got %+v", v)
	}
}

func TestCheckPermissions_MetacharRejectsWithoutDecomposition(t *testing.T) {
	env := DefaultEnvelope()
	env.Permissions.Shell = true
	env.Permissions.ShellAllowlist = []string{"git", "ls"}
	plan := ActionPlan{Steps: []Step{stepRaw("s1", "shell", map[string]interface{}{"command": "git status && ls"})}}
	v := CheckPermissions(plan, env, false)
	if len(v) != 1 {
		t.Fatalf("want one violation (metachar reject, decomposition not opted in), got %+v", v)
	}
}

func TestCheckPermissions_DecompositionOptIn(t *testing.T) {
	env := DefaultEnvelope()
	env.Permissions.Shell = true
	env.Permissions.ShellAllowDecomposition = true
	env.Permissions.ShellAllowlist = []string{"git", "ls"}
	plan := ActionPlan{Steps: []Step{stepRaw("s1", "shell", map[string]interface{}{"command": "git status && ls"})}}
	v := CheckPermissions(plan, env, false)
	if len(v) != 0 {
		t.Fatalf("want zero violations under decomposition opt-in, got %+v", v)
	}
}

func TestCheckPermissions_FileScopes(t *testing.T) {
	env := DefaultEnvelope()
	env.Permissions.FileRead = []string{"src/**"}
	env.Permissions.FileWrite = []string{}
	plan := ActionPlan{Steps: []Step{
		stepRaw("r1", "file_read", map[string]interface{}{"path": "src/main.go"}),
		stepRaw("r2", "file_read", map[string]interface{}{"path": "/etc/passwd"}),
		stepRaw("w1", "file_write", map[string]interface{}{"path": "out.txt", "content": "x"}),
	}}
	v := CheckPermissions(plan, env, false)
	if len(v) != 2 {
		t.Fatalf("want 2 violations (r2 outside scope, w1 no write scope), got %+v", v)
	}
}

func TestCheckDAG_Cycle(t *testing.T) {
	plan := ActionPlan{Steps: []Step{
		{ID: "a", Type: "task", DependsOn: []string{"b"}, Raw: map[string]interface{}{"id": "a", "depends_on": []interface{}{"b"}}},
		{ID: "b", Type: "task", DependsOn: []string{"a"}, Raw: map[string]interface{}{"id": "b", "depends_on": []interface{}{"a"}}},
	}}
	v := CheckDAG(plan)
	if len(v) != 1 || v[0].Stage != "dag" || v[0].HasStep {
		t.Fatalf("want one dag violation with step=null, got %+v", v)
	}
}

func TestCheckDAG_MissingDep(t *testing.T) {
	plan := ActionPlan{Steps: []Step{
		{ID: "a", Type: "task", DependsOn: []string{"ghost"}, Raw: map[string]interface{}{"id": "a"}},
	}}
	v := CheckDAG(plan)
	if len(v) != 1 || v[0].Step != "a" {
		t.Fatalf("want one dag violation on a, got %+v", v)
	}
}

func TestCheckDAG_DuplicateID(t *testing.T) {
	plan := ActionPlan{Steps: []Step{
		{ID: "a", Type: "task", Raw: map[string]interface{}{"id": "a"}},
		{ID: "a", Type: "task", Raw: map[string]interface{}{"id": "a"}},
	}}
	v := CheckDAG(plan)
	if len(v) != 1 || v[0].Step != "a" {
		t.Fatalf("want one dag violation on the duplicate id, got %+v", v)
	}
}

func TestCheckDAG_Clean(t *testing.T) {
	plan := ActionPlan{Steps: []Step{
		{ID: "a", Type: "task", Raw: map[string]interface{}{"id": "a"}},
		{ID: "b", Type: "task", DependsOn: []string{"a"}, Raw: map[string]interface{}{"id": "b"}},
	}}
	if v := CheckDAG(plan); len(v) != 0 {
		t.Fatalf("want zero violations, got %+v", v)
	}
}

func TestDelegationSafety_PhysicalStakesBlocksPreferredModel(t *testing.T) {
	env := DefaultEnvelope()
	env.Stakes = "physical"
	plan := ActionPlan{Steps: []Step{
		stepRaw("s1", "task", map[string]interface{}{"prompt": "x", "preferred_model": "haiku"}),
	}}
	v := checkDelegationSafety(plan, env)
	if len(v) != 1 || v[0].Step != "s1" {
		t.Fatalf("want one delegation-safety violation, got %+v", v)
	}
}

func TestDelegationSafety_PhysicalStakesBlocksAgentic(t *testing.T) {
	env := DefaultEnvelope()
	env.Stakes = "physical"
	plan := ActionPlan{Steps: []Step{stepRaw("s1", "agentic", map[string]interface{}{})}}
	v := checkDelegationSafety(plan, env)
	if len(v) != 1 || v[0].Step != "s1" {
		t.Fatalf("want one delegation-safety violation for an agentic step, got %+v", v)
	}
}

func TestResolveStrict_DefaultsFromStakes(t *testing.T) {
	for _, tc := range []struct {
		stakes string
		want   bool
	}{{"low", false}, {"medium", false}, {"high", true}, {"physical", true}} {
		env := Envelope{Stakes: tc.stakes}
		if got := ResolveStrict(nil, env); got != tc.want {
			t.Errorf("stakes=%q: ResolveStrict(nil,...) = %v, want %v", tc.stakes, got, tc.want)
		}
	}
}

func TestCheckAgenticStep_NoToolsRejected(t *testing.T) {
	env := DefaultEnvelope()
	plan := ActionPlan{Steps: []Step{
		stepRaw("s1", "agentic", map[string]interface{}{"workspace": "/w", "tools": []interface{}{}}),
	}}
	v := CheckPermissions(plan, env, false)
	if len(v) != 1 {
		t.Fatalf("want one violation (no tools), got %+v", v)
	}
}

func TestCheckAgenticStep_UngrantedCapability(t *testing.T) {
	env := DefaultEnvelope()
	env.Permissions.FileRead = []string{"/w/**"}
	plan := ActionPlan{Steps: []Step{
		stepRaw("s1", "agentic", map[string]interface{}{
			"workspace": "/w", "tools": []interface{}{"Bash"}, // shell not granted
		}),
	}}
	v := CheckPermissions(plan, env, false)
	if len(v) != 1 {
		t.Fatalf("want one violation (Bash needs shell, not granted), got %+v", v)
	}
}
