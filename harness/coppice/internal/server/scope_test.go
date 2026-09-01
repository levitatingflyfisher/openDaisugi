package server

import (
	"reflect"
	"sort"
	"testing"
)

// scopeTeam makes a team with two child tasks, each with one pane, and a
// loose pane that works for no task. It returns the team id, the two
// child ids, and the three pane ids: the two in the team, then the loose
// one.
func scopeTeam(t *testing.T, s *Server) (team string, kids []string, panes []string) {
	t.Helper()
	team, _ = createTask(t, s, `"label":"review-team","cwd":"/"`)
	for _, label := range []string{"docs", "tests"} {
		kid, _ := createTask(t, s, `"label":"`+label+`","parent":"`+team+`","cwd":"/"`)
		kids = append(kids, kid)
		got := roundTrip(t, s,
			`{"id":"1","cmd":"pane.create","task":"`+kid+`","cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
		id, _ := result(t, got[0])["pane"].(string)
		panes = append(panes, id)
	}
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"/","cmd_argv":["sh","-c","sleep 30"],"kind":"pty"}`)
	id, _ := result(t, got[0])["pane"].(string)
	panes = append(panes, id)
	return team, kids, panes
}

// ids reads the id of every row in a list result.
func ids(rows any) []string {
	list, _ := rows.([]any)
	out := []string{}
	for _, r := range list {
		m, _ := r.(map[string]any)
		id, _ := m["id"].(string)
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// pathOf reads the path of a list result.
func pathOf(m map[string]any) []string {
	raw, _ := m["path"].([]any)
	out := []string{}
	for _, p := range raw {
		s, _ := p.(string)
		out = append(out, s)
	}
	return out
}

func TestPaneListWithAScopeListsOnlyThatSubtree(t *testing.T) {
	s := newPaneServer(t)
	team, _, panes := scopeTeam(t, s)

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.list","scope":"`+team+`"}`)
	m := result(t, got[0])
	want := append([]string{}, panes[:2]...)
	sort.Strings(want)
	if g := ids(m["panes"]); !reflect.DeepEqual(g, want) {
		t.Fatalf("scoped panes = %v, want %v", g, want)
	}
	if p := pathOf(m); !reflect.DeepEqual(p, []string{"floor", "review-team"}) {
		t.Fatalf("path = %v", p)
	}

	got = roundTrip(t, s, `{"id":"2","cmd":"pane.list"}`)
	m = result(t, got[0])
	all := append([]string{}, panes...)
	sort.Strings(all)
	if g := ids(m["panes"]); !reflect.DeepEqual(g, all) {
		t.Fatalf("unscoped panes = %v, want %v", g, all)
	}
	if p := pathOf(m); !reflect.DeepEqual(p, []string{"floor"}) {
		t.Fatalf("unscoped path = %v", p)
	}
}

func TestTaskListWithAScopeListsTheTaskAndItsDescendants(t *testing.T) {
	s := newPaneServer(t)
	team, kids, _ := scopeTeam(t, s)
	other, _ := createTask(t, s, `"label":"other","cwd":"/"`)

	got := roundTrip(t, s, `{"id":"1","cmd":"task.list","scope":"`+kids[1]+`"}`)
	m := result(t, got[0])
	if g := ids(m["tasks"]); !reflect.DeepEqual(g, []string{kids[1]}) {
		t.Fatalf("scoped tasks = %v", g)
	}
	if p := pathOf(m); !reflect.DeepEqual(p, []string{"floor", "review-team", "tests"}) {
		t.Fatalf("path = %v", p)
	}

	got = roundTrip(t, s, `{"id":"2","cmd":"task.list","scope":"`+team+`"}`)
	want := append([]string{team}, kids...)
	sort.Strings(want)
	if g := ids(result(t, got[0])["tasks"]); !reflect.DeepEqual(g, want) {
		t.Fatalf("team tasks = %v, want %v", g, want)
	}

	got = roundTrip(t, s, `{"id":"3","cmd":"task.list"}`)
	all := append([]string{team, other}, kids...)
	sort.Strings(all)
	if g := ids(result(t, got[0])["tasks"]); !reflect.DeepEqual(g, all) {
		t.Fatalf("all tasks = %v, want %v", g, all)
	}
}

// A scope that names no task is refused. An empty list would read as a
// quiet team.
func TestAScopeThatNamesNoTaskIsRefused(t *testing.T) {
	s := newPaneServer(t)
	for _, cmd := range []string{"pane.list", "task.list"} {
		got := roundTrip(t, s, `{"id":"1","cmd":"`+cmd+`","scope":"t99"}`)
		if got[0].OK || got[0].Error == nil || got[0].Error.Code != "bad_request" {
			t.Fatalf("%s with scope t99 = %+v, want bad_request", cmd, got[0])
		}
	}
}
