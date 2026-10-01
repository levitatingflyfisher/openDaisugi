//go:build unix

package server

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// machinesFixtures puts the fake tailscale (the web cases' own) first on
// PATH, with three peers: gpu (online, tailnet address 127.0.0.1), laptop
// (online, no profile) and nas (offline). It never runs the real
// tailscale: PATH holds only the fakes and the system directories, and the
// fake shadows the real one.
func machinesFixtures(t *testing.T, bundle string) {
	t.Helper()
	dir := t.TempDir()
	fake, err := os.ReadFile("../../../coppice-rs/cases/web/fixtures/bin/tailscale")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	if err := os.WriteFile(filepath.Join(bin, "tailscale"), fake, 0o755); err != nil {
		t.Fatal(err)
	}
	peers := filepath.Join(dir, "peers.json")
	_ = os.WriteFile(peers, []byte(`{
 "nodekey:a": {"HostName":"GPU","DNSName":"gpu.tail1.ts.net.","OS":"linux","Online":true,"LastSeen":"0001-01-01T00:00:00Z","TailscaleIPs":["127.0.0.1","fd7a::1"]},
 "nodekey:b": {"HostName":"laptop","DNSName":"laptop.tail1.ts.net.","OS":"macOS","Online":true,"TailscaleIPs":["127.0.0.4"]},
 "nodekey:c": {"HostName":"nas","DNSName":"nas.tail1.ts.net.","OS":"linux","Online":false,"LastSeen":"2026-10-01T10:00:00Z","TailscaleIPs":["127.0.0.5"]}
}`), 0o644)
	daisugi := filepath.Join(bin, "daisugi")
	_ = os.WriteFile(daisugi, []byte("#!/bin/sh\nprintf '%s' '"+bundle+"'\n"), 0o755)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("COPPICE_DAISUGI_BIN", daisugi)
	t.Setenv("FAKE_TS_PEERS_FILE", peers)
	t.Setenv("FAKE_TS_IPS", "127.0.0.2")
}

func listenPort(t *testing.T) (net.Listener, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return l, strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func closedPort(t *testing.T) string {
	t.Helper()
	l, port := listenPort(t)
	_ = l.Close()
	return port
}

func machineNamed(t *testing.T, m map[string]any, name string) map[string]any {
	t.Helper()
	for _, x := range m["machines"].([]any) {
		row := x.(map[string]any)
		if row["name"] == name {
			return row
		}
	}
	t.Fatalf("no machine %q in %v", name, m["machines"])
	return nil
}

// Each machine lists the profiles that point at it, matched by MagicDNS
// name or tailnet address, with a TCP answer time; a profile whose server
// does not answer says so. Keys never appear in the reply.
func TestFloorMachinesListsProfilesPerMachine(t *testing.T) {
	_, up := listenPort(t)
	down := closedPort(t)
	machinesFixtures(t, `[{"base_url":"https://api.example.com","key_set":true,"model":null,"name":"cloud","wire":"anthropic"},`+
		`{"base_url":"http://127.0.0.5:`+down+`/v1","key_set":false,"model":"m","name":"dead","wire":"openai"},`+
		`{"base_url":"http://gpu.tail1.ts.net:`+up+`/v1","key_set":true,"model":"m","name":"desk","wire":"openai"},`+
		`{"base_url":null,"key_set":true,"model":"haiku","name":"hk","wire":"claude-code"}]`)
	s := newTestServer(t)
	raw := roundTrip(t, s, `{"id":"1","cmd":"floor.machines"}`)[0]
	m := result(t, raw)
	if m["tailscale"] != "ok" {
		t.Fatalf("tailscale = %v", m["tailscale"])
	}
	rows := m["machines"].([]any)
	if len(rows) != 4 || rows[0].(map[string]any)["name"] != "box" || rows[0].(map[string]any)["self"] != true {
		t.Fatalf("machines = %v", rows)
	}
	gpu := machineNamed(t, m, "gpu")
	if gpu["online"] != true || gpu["os"] != "linux" || gpu["self"] != false {
		t.Fatalf("gpu = %v", gpu)
	}
	if _, has := gpu["last_seen"]; has {
		t.Fatalf("a zero last seen is not shown: %v", gpu)
	}
	p := gpu["profiles"].([]any)
	if len(p) != 1 {
		t.Fatalf("gpu profiles = %v", p)
	}
	desk := p[0].(map[string]any)
	if desk["name"] != "desk" || desk["address"] != "gpu.tail1.ts.net:"+up || desk["answers"] != true || desk["answers_ms"] == nil {
		t.Fatalf("desk = %v", desk)
	}
	nas := machineNamed(t, m, "nas")
	if nas["online"] != false || nas["last_seen"] != "2026-10-01T10:00:00Z" {
		t.Fatalf("nas = %v", nas)
	}
	dead := nas["profiles"].([]any)[0].(map[string]any)
	if dead["name"] != "dead" || dead["answers"] != false || dead["answers_ms"] != nil {
		t.Fatalf("dead = %v", dead)
	}
	if got := machineNamed(t, m, "laptop")["profiles"].([]any); len(got) != 0 {
		t.Fatalf("laptop profiles = %v", got)
	}
	if desk["key_set"] != true || dead["key_set"] != false {
		t.Fatalf("key_set: desk %v, dead %v", desk["key_set"], dead["key_set"])
	}
	b, _ := json.Marshal(raw.Result)
	if strings.Contains(string(b), `"hk"`) || strings.Contains(string(b), "cloud") {
		t.Fatalf("a profile with no machine is listed: %s", b)
	}
}

// A voice server named by [voice] url on a machine is listed with the
// same probe.
func TestFloorMachinesListsAVoiceURL(t *testing.T) {
	_, up := listenPort(t)
	machinesFixtures(t, `[]`)
	s := newTestServer(t)
	s.cfg.Voice.URL = "http://laptop:" + up
	m := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.machines"}`)[0])
	v, ok := machineNamed(t, m, "laptop")["voice"].(map[string]any)
	if !ok || v["address"] != "laptop:"+up || v["answers"] != false {
		// laptop's tailnet address 127.0.0.4 has no listener here.
		t.Fatalf("voice = %v", machineNamed(t, m, "laptop"))
	}
}

// A speech server named by [voice] speak_url on a machine is listed with the
// same probe, under speak.
func TestFloorMachinesListsASpeakURL(t *testing.T) {
	_, up := listenPort(t)
	machinesFixtures(t, `[]`)
	s := newTestServer(t)
	s.cfg.Voice.SpeakURL = "http://laptop:" + up + "/v1"
	m := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.machines"}`)[0])
	v, ok := machineNamed(t, m, "laptop")["speak"].(map[string]any)
	if !ok || v["address"] != "laptop:"+up {
		t.Fatalf("speak = %v", machineNamed(t, m, "laptop"))
	}
	if _, has := machineNamed(t, m, "gpu")["speak"]; has {
		t.Fatal("a machine with no speech server shows one")
	}
}

// A profile that points at this box by a loopback name is listed on the
// self row.
func TestFloorMachinesLoopbackProfileIsThisBox(t *testing.T) {
	_, up := listenPort(t)
	machinesFixtures(t, `[{"base_url":"http://localhost:`+up+`/v1","key_set":true,"model":null,"name":"local","wire":"openai"}]`)
	s := newTestServer(t)
	m := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.machines"}`)[0])
	p := machineNamed(t, m, "box")["profiles"].([]any)
	if len(p) != 1 || p[0].(map[string]any)["answers"] != true {
		t.Fatalf("box profiles = %v", p)
	}
}

// No tailscale on PATH: absent, no machines, one plain line.
func TestFloorMachinesAbsentTailscale(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	s := newTestServer(t)
	m := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.machines"}`)[0])
	if m["tailscale"] != "absent" || len(m["machines"].([]any)) != 0 {
		t.Fatalf("reply = %v", m)
	}
	if m["line"] != MachinesAbsentLine {
		t.Fatalf("line = %v", m["line"])
	}
}

// Tailscale that does not run, or is not in the Running state: off.
func TestFloorMachinesOffTailscale(t *testing.T) {
	for _, env := range [][2]string{{"FAKE_TS_FAIL", "1"}, {"FAKE_TS_STATE", "Stopped"}} {
		machinesFixtures(t, `[]`)
		t.Setenv(env[0], env[1])
		s := newTestServer(t)
		m := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.machines"}`)[0])
		if m["tailscale"] != "off" || len(m["machines"].([]any)) != 0 || m["line"] != MachinesOffLine {
			t.Fatalf("%v: reply = %v", env, m)
		}
	}
}

// A profile table that cannot be read is said in one line, never as "no
// profile points here".
func TestFloorMachinesSaysWhenProfilesAreUnknown(t *testing.T) {
	machinesFixtures(t, `[]`)
	t.Setenv("COPPICE_DAISUGI_BIN", filepath.Join(t.TempDir(), "missing"))
	s := newTestServer(t)
	m := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.machines"}`)[0])
	if m["tailscale"] != "ok" || m["profiles_line"] != MachinesProfilesLine {
		t.Fatalf("reply = %v", m)
	}
}

// A tailscale that hangs is cut at 3 s, with its whole group, and the
// answer is off.
func TestFloorMachinesCutsAHangingTailscale(t *testing.T) {
	machinesFixtures(t, `[]`)
	t.Setenv("FAKE_TS_SLEEP", "30")
	s := newTestServer(t)
	start := time.Now()
	m := result(t, roundTrip(t, s, `{"id":"1","cmd":"floor.machines"}`)[0])
	if m["tailscale"] != "off" || time.Since(start) > 6*time.Second {
		t.Fatalf("reply = %v after %v", m, time.Since(start))
	}
}

// A pane and a plugin are refused, as for the foreman.
func TestFloorMachinesRefusesAPane(t *testing.T) {
	machinesFixtures(t, `[]`)
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"hello","role":"pane","pane":"w1:p1"}`, `{"id":"2","cmd":"floor.machines"}`)
	if got[1].OK || got[1].Error == nil || got[1].Error.Message != MachinesRefusal {
		t.Fatalf("reply = %+v", got[1])
	}
}
