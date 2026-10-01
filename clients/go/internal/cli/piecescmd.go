package cli

import (
	"os"
	"strings"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is the pieces half of opendaisugi.modules: `daisugi modules
// --pieces`, the six pieces and whether each is on here.

var piecesOpt = opt{names: []string{"--pieces"}, help: "Show the six pieces: on or off here, and the command for each."}

// piece is modules.Piece.
type piece struct {
	key, title, state, detail, turnOn, turnOff string
}

// mcpRegistered is modules._mcp_registered: the opendaisugi server is
// among the mcpServers of ~/.claude.json.
func (e *Env) mcpRegistered() bool {
	raw, err := os.ReadFile(gateroot.Join(e.home, ".claude.json"))
	if err != nil {
		return false
	}
	text, exc := pystr.DecodeStrict(raw)
	if exc != nil {
		return false
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return false
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return false
	}
	s, has := o.Get("mcpServers")
	if !has {
		return false
	}
	so, ok := s.(*pyjson.Object)
	if !ok {
		return false
	}
	_, has = so.Get("opendaisugi")
	return has
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func pick(on bool, yes, no string) string {
	if on {
		return yes
	}
	return no
}

// detectPieces is modules.detect_pieces.
func (e *Env) detectPieces(cmd, dataDir string) ([]piece, error) {
	cfg, err := config.Load(gateroot.Join(dataDir, "config.yaml"))
	if err != nil {
		return nil, e.configLoadErr(cmd, err)
	}
	hooks, herr := readHookSettings(gateroot.Join(e.home, ".claude/settings.json"))
	if herr != nil {
		return nil, e.refuse(cmd, herr)
	}
	which := func(name string) bool {
		_, err := install.LookPath(e.env)(name)
		return err == nil
	}
	var gateFor []string
	if hooks.installed("daisugi hook", "PreToolUse") {
		gateFor = append(gateFor, "claude-code")
	}
	if exists(gateroot.Join(e.home, ".pi/agent/extensions/daisugi-gate/index.ts")) {
		gateFor = append(gateFor, "pi")
	}
	if exists(e.opencodePluginPath()) {
		gateFor = append(gateFor, "opencode")
	}
	gatewayOn := e.env["OPENDAISUGI_GATEWAY_BASE_URL"] != "" || hooks.installed("gateway", "PreToolUse")
	router := cfg.GatewayRouter
	routerDetail := "gateway_router: " + router
	routerState := "off"
	if gatewayOn {
		routerState = router
	} else {
		routerDetail += "; it runs only when the gateway is on"
	}
	mcp := e.mcpRegistered()
	floor := e.coppiceSocketPresent()
	loop := which("sprig")
	return []piece{
		{"gate", "gate", onOff(len(gateFor) > 0),
			pick(len(gateFor) > 0, "hook installed for "+strings.Join(gateFor, ", "), "no gate hook found"),
			"daisugi install --gate", "daisugi install --uninstall (removes gate, gateway and memory together)"},
		{"gateway", "gateway", onOff(gatewayOn),
			pick(gatewayOn, "the harness base_url points at it", "no harness points at it"),
			"daisugi install --gateway", "daisugi install --uninstall (removes gate, gateway and memory together)"},
		{"router", "router", routerState, routerDetail,
			"daisugi install --gateway --router rules|switchyard|knn|model", "daisugi install --gateway --router off"},
		{"memory", "memory and pathways", onOff(mcp),
			pick(mcp, "MCP server registered", "MCP server not registered"),
			"daisugi install", "daisugi install --uninstall (removes gate, gateway and memory together)"},
		{"floor", "floor (coppice)", onOff(floor),
			pick(floor, "server answers", "no server answers"),
			"coppice server start", "coppice server stop"},
		{"loop", "loop (sprig)", onOff(loop),
			pick(loop, "sprig is on PATH", "sprig is not on PATH"),
			"scripts/install.sh", "not managed by daisugi: uninstall sprig yourself"},
	}, nil
}

// renderPieces is modules.render_pieces.
func renderPieces(dataDir string, ps []piece) string {
	out := []string{"openDaisugi: pieces   (data dir: " + dataDir + ")", ""}
	pad := strings.Repeat(" ", 22)
	for _, p := range ps {
		out = append(out, pyLjust(p.title, 22)+pyLjust(p.state, 4)+"  "+p.detail,
			pad+"on:   "+p.turnOn, pad+"off:  "+p.turnOff)
	}
	out = append(out, "", "No piece needs another. Turn on only the ones you want.")
	return strings.Join(out, "\n")
}

// piecesJSON is modules.pieces_json.
func piecesJSON(ps []piece) string {
	out := make([]*pyjson.Object, 0, len(ps))
	for _, p := range ps {
		out = append(out, pyjson.NewObject().Set("key", p.key).Set("title", p.title).Set("state", p.state).
			Set("detail", p.detail).Set("turn_on", p.turnOn).Set("turn_off", p.turnOff))
	}
	return pyjson.DumpsIndent(out, 2, true)
}
