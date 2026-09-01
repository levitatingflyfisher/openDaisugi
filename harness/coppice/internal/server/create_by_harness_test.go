package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// saveFakeHarness points XDG_CONFIG_HOME at a temp dir and writes a config
// whose one harness, fakeh, runs sh -c 'sleep 30'.
func saveFakeHarness(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
}

// createByHarness sends pane.create with harness and no cmd_argv and
// returns the raw reply.
func createByHarness(t *testing.T, s *Server, extra string) map[string]any {
	t.Helper()
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","harness":"fakeh","cwd":"/"`+extra+`}`)
	if !got[0].OK {
		return map[string]any{"error": got[0].Error}
	}
	return result(t, got[0])
}

func closePane(t *testing.T, s *Server, id string) {
	t.Helper()
	roundTrip(t, s, `{"id":"1","cmd":"pane.close","pane":"`+id+`"}`)
}

func TestCreateByHarnessBuildsArgvFromTheConfig(t *testing.T) {
	toolchain.RequireOrSkip(t)
	saveFakeHarness(t)
	s := newTestServer(t)
	res := createByHarness(t, s, "")
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create returned no pane: %v", res)
	}
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if closed, _ := row["closed"].(bool); closed {
		t.Fatalf("pane is not live: %v", row)
	}
	if row["harness"] != "fakeh" {
		t.Fatalf("harness = %v, want fakeh", row["harness"])
	}
	if !reflect.DeepEqual(row["cmd"], []any{"sh", "-c", "sleep 30"}) {
		t.Fatalf("cmd = %v", row["cmd"])
	}
}

func TestCreateByHarnessWithNoConfigIsBadRequest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","harness":"fakeh","cwd":"/"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != "bad_request" {
		t.Fatalf("want bad_request, got %+v", got[0])
	}
	msg := got[0].Error.Message
	if !strings.Contains(msg, `no harness named "fakeh"`) || !strings.Contains(msg, config.Path()) {
		t.Fatalf("message = %q", msg)
	}
	if !strings.Contains(msg, `[harness.fakeh]`) || !strings.Contains(msg, `command = "fakeh"`) {
		t.Fatalf("message does not teach the table: %q", msg)
	}
}

func TestCreateByHarnessWithATableButNoCommandIsBadRequest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Harness: map[string]config.Harness{"fakeh": {Args: []string{"-x"}}}}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","harness":"fakeh","cwd":"/"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != "bad_request" {
		t.Fatalf("want bad_request, got %+v", got[0])
	}
	if !strings.Contains(got[0].Error.Message, `no harness named "fakeh"`) {
		t.Fatalf("message = %q", got[0].Error.Message)
	}
}

func TestCreateByHarnessAppendsTheRequestArgs(t *testing.T) {
	toolchain.RequireOrSkip(t)
	saveFakeHarness(t)
	s := newTestServer(t)
	res := createByHarness(t, s, `,"args":["-x"]`)
	id, _ := res["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create returned no pane: %v", res)
	}
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if !reflect.DeepEqual(row["cmd"], []any{"sh", "-c", "sleep 30", "-x"}) {
		t.Fatalf("cmd = %v", row["cmd"])
	}
}

// A pty pane with neither cmd_argv nor harness keeps the old refusal.
func TestCreateWithNeitherArgvNorHarnessStillRefuses(t *testing.T) {
	saveFakeHarness(t)
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"/"}`)
	if got[0].OK || got[0].Error == nil || !strings.Contains(got[0].Error.Message, "a pty pane needs cmd_argv") {
		t.Fatalf("got %+v", got[0])
	}
}

// cmd_argv wins when both are given: the config is only read when the
// request gives no command of its own.
func TestCreateWithArgvAndHarnessKeepsTheArgv(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","harness":"fakeh","cwd":"/","cmd_argv":["sh","-c","sleep 30"]}`)
	id, _ := result(t, got[0])["pane"].(string)
	if id == "" {
		t.Fatalf("no pane: %+v", got[0])
	}
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["harness"] != "fakeh" || !reflect.DeepEqual(row["cmd"], []any{"sh", "-c", "sleep 30"}) {
		t.Fatalf("row = %v", row)
	}
}
