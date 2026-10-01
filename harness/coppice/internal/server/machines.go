package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// floor.machines: a read-only view of the boxes on the tailnet. Which are
// up, what each serves, how fast each answers, and which profiles point at
// it. It runs `tailscale status --json` once per call, reads the operator's
// agent profiles with `daisugi agent-profiles list --json`, which names them and
// their base URLs and says whether each key is set (never a key), and
// opens one TCP connection to each profile's host and port. Adding a box
// and the access rules stay in Tailscale's own console.

// MachinesRefusal is what a pane or a plugin gets for floor.machines. The
// tailnet's boxes and the profiles that point at them are the owner's.
const MachinesRefusal = "only the operator reads the machines."

// MachinesAbsentLine and MachinesOffLine are the one line the Machines view
// shows when tailscale is not on this box, or does not run.
const (
	MachinesAbsentLine = "Tailscale is not on this box. Install Tailscale, then open Machines again."
	MachinesOffLine    = "Tailscale is not running on this box. Run tailscale up, then open Machines again."
	// MachinesProfilesLine says the profile table could not be read, so an
	// empty profile list is not a fact.
	MachinesProfilesLine = "The agent profiles are not known. Check that daisugi runs, then open Machines again."
)

const (
	machinesStatusTimeout = 3 * time.Second
	machinesBundleTimeout = 3 * time.Second
	machinesDialTimeout   = time.Second
)

// tsPeer is one node in tailscale status --json: this box (Self) or a
// peer. The names are tailscale's ipnstate.PeerStatus, which has no json
// tags.
type tsPeer struct {
	HostName     string
	DNSName      string
	OS           string
	Online       bool
	LastSeen     string
	TailscaleIPs []string
}

type tsMachines struct {
	BackendState string
	Self         *tsPeer
	Peer         map[string]tsPeer
}

// machineProbe is a profile, or a voice server, that points at a machine,
// and whether its host and port answer a TCP connection.
type machineProbe struct {
	Name      string `json:"name,omitempty"`
	Address   string `json:"address"`
	Answers   bool   `json:"answers"`
	AnswersMS *int   `json:"answers_ms"`
	// KeySet is false when a profile names a key variable that is not set.
	// A voice server has none.
	KeySet *bool `json:"key_set,omitempty"`

	dial string
}

type machineRow struct {
	Name     string          `json:"name"`
	Online   bool            `json:"online"`
	OS       string          `json:"os"`
	Self     bool            `json:"self"`
	LastSeen string          `json:"last_seen,omitempty"`
	Profiles []*machineProbe `json:"profiles"`
	Voice    *machineProbe   `json:"voice,omitempty"`
	// Speak is the speech server of [voice] speak_url, when it is here.
	Speak *machineProbe `json:"speak,omitempty"`

	names []string
	ips   []string
}

// shortName is the MagicDNS short name of a node: the first label of its
// DNS name, else its host name, in lower case.
func shortName(p tsPeer) string {
	dns := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(p.DNSName)), ".")
	if i := strings.IndexByte(dns, '.'); i > 0 {
		dns = dns[:i]
	}
	if dns != "" {
		return dns
	}
	return strings.ToLower(strings.TrimSpace(p.HostName))
}

func newMachineRow(p tsPeer, self bool) *machineRow {
	r := &machineRow{Name: shortName(p), Online: p.Online, OS: p.OS, Self: self, Profiles: []*machineProbe{}, ips: p.TailscaleIPs}
	if ls := strings.TrimSpace(p.LastSeen); ls != "" && !strings.HasPrefix(ls, "0001-01-01") {
		r.LastSeen = ls
	}
	r.names = []string{r.Name}
	if full := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(p.DNSName)), "."); full != "" {
		r.names = append(r.names, full)
	}
	return r
}

// has reports whether host (lower case, no brackets) names the machine:
// its short or full MagicDNS name, or one of its tailnet addresses.
func (r *machineRow) has(host string) bool {
	for _, n := range r.names {
		if n == host {
			return true
		}
	}
	for _, ip := range r.ips {
		if strings.EqualFold(ip, host) {
			return true
		}
	}
	return false
}

// dialHost is the host to connect to for a probe whose URL names host: the
// machine's first IPv4 tailnet address when the URL uses a MagicDNS name
// (so the check needs no DNS), else the host as written.
func (r *machineRow) dialHost(host string) string {
	for _, n := range r.names {
		if n == host {
			for _, ip := range r.ips {
				if p := net.ParseIP(ip); p != nil && p.To4() != nil {
					return ip
				}
			}
		}
	}
	return host
}

// loopbackHost is true for the names that mean this box.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostPort reads a base URL: its lower-case host, its port (the scheme's
// default when none), and false for a URL with no host or a scheme that has
// no default port and names none.
func hostPort(raw string) (host, port string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", "", false
	}
	port = u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", "", false
		}
	}
	return strings.ToLower(u.Hostname()), port, true
}

// machinesFrom turns a tailscale status reply into rows: this box first,
// then the peers by name.
func machinesFrom(st tsMachines) []*machineRow {
	var rows []*machineRow
	if st.Self != nil {
		rows = append(rows, newMachineRow(*st.Self, true))
	}
	var peers []*machineRow
	for _, p := range st.Peer {
		peers = append(peers, newMachineRow(p, false))
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Name != peers[j].Name {
			return peers[i].Name < peers[j].Name
		}
		return strings.Join(peers[i].ips, ",") < strings.Join(peers[j].ips, ",")
	})
	return append(rows, peers...)
}

// readTailnet runs tailscale status --json once. state is "ok", "absent"
// (no tailscale on PATH) or "off" (it does not answer, or is not Running).
func readTailnet() (state string, st tsMachines) {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return "absent", st
	}
	ctx, cancel := context.WithTimeout(context.Background(), machinesStatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "status", "--json")
	// A group of its own that dies with the limit, and a read that ends
	// with the command, as bundleBytes does, so a down tailscale that
	// leaves a child holding the pipe never holds the call.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var buf bytes.Buffer
	cmd.Stdout = &limitedBuffer{b: &buf, max: 1 << 20}
	err = cmd.Run()
	out := buf.Bytes()
	if err != nil || json.Unmarshal(out, &st) != nil || st.BackendState != "Running" || st.Self == nil {
		return "off", tsMachines{}
	}
	return "ok", st
}

// profileRow is one row of `daisugi agent-profiles list --json`: a valid
// profile, never its key. KeySet is false only when the profile names a key
// variable that is not set.
type profileRow struct {
	Name    string  `json:"name"`
	BaseURL *string `json:"base_url"`
	KeySet  bool    `json:"key_set"`
}

// profileRows runs daisugi agent-profiles list --json in the foreman's
// neutral compactor directory: in a group of its own that dies with the
// limit, its read capped at 1 MiB and ended with the command. A profile
// with no base URL (the default, a Claude Code wire) is left out.
func (s *Server) profileRows() ([]profileRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), machinesBundleTimeout)
	defer cancel()
	if err := os.MkdirAll(s.compactorDir(), 0o700); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, daisugiBin(), "agent-profiles", "list", "--json")
	cmd.Dir = s.compactorDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &limitedBuffer{b: &out, max: 1 << 20}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var rows []profileRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		return nil, err
	}
	kept := rows[:0]
	for _, r := range rows {
		if r.BaseURL != nil && *r.BaseURL != "" {
			kept = append(kept, r)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	return kept, nil
}

// locate finds the machine a base URL names, and a probe for its host and
// port. A loopback host names this box.
func locate(rows []*machineRow, raw string) (*machineRow, *machineProbe) {
	host, port, ok := hostPort(raw)
	if !ok {
		return nil, nil
	}
	for _, r := range rows {
		if r.has(host) {
			return r, &machineProbe{Address: host + ":" + port, dial: net.JoinHostPort(r.dialHost(host), port)}
		}
	}
	// Only a name no machine claims falls back to this box.
	if loopbackHost(host) && len(rows) > 0 && rows[0].Self {
		return rows[0], &machineProbe{Address: host + ":" + port, dial: net.JoinHostPort(host, port)}
	}
	return nil, nil
}

func (s *Server) handleFloorMachines(_ *Client, r *proto.Request) proto.Response {
	state, st := readTailnet()
	res := map[string]any{"tailscale": state, "machines": []*machineRow{}}
	switch state {
	case "absent":
		res["line"] = MachinesAbsentLine
		return proto.OKResp(r.ID, res)
	case "off":
		res["line"] = MachinesOffLine
		return proto.OKResp(r.ID, res)
	}
	rows := machinesFrom(st)
	res["machines"] = rows
	if profiles, err := s.profileRows(); err != nil {
		res["profiles_line"] = MachinesProfilesLine
	} else {
		for _, pr := range profiles {
			if row, p := locate(rows, *pr.BaseURL); row != nil {
				p.Name = pr.Name
				keySet := pr.KeySet
				p.KeySet = &keySet
				row.Profiles = append(row.Profiles, p)
			}
		}
	}
	if u := strings.TrimSpace(s.cfg.Voice.URL); u != "" {
		if row, p := locate(rows, u); row != nil {
			row.Voice = p
		}
	}
	if u := strings.TrimSpace(s.cfg.Voice.SpeakURL); u != "" {
		if row, p := locate(rows, u); row != nil {
			row.Speak = p
		}
	}
	var probes []*machineProbe
	for _, row := range rows {
		probes = append(probes, row.Profiles...)
		if row.Voice != nil {
			probes = append(probes, row.Voice)
		}
		if row.Speak != nil {
			probes = append(probes, row.Speak)
		}
	}
	var wg sync.WaitGroup
	for _, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			conn, err := net.DialTimeout("tcp", p.dial, machinesDialTimeout)
			if err != nil {
				return
			}
			_ = conn.Close()
			ms := int(time.Since(start) / time.Millisecond)
			p.Answers, p.AnswersMS = true, &ms
		}()
	}
	wg.Wait()
	return proto.OKResp(r.ID, res)
}

// RegisterMachinesCommands wires up floor.machines.
func (s *Server) RegisterMachinesCommands() {
	_ = s.Handle("floor.machines", s.handleFloorMachines)
}
