package sprig

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// CLI is sprig's automatable entry point, built to clig.dev: the answer goes to
// stdout, everything else to stderr; --json for machines; real exit codes; the
// task comes from an argument or stdin. It is non-interactive by design — one
// task in, one answer out — so it drops straight into cron, CI, and pipes.
type CLI struct {
	Version  string
	NewModel func(ModelOptions) (Model, error) // the pluggable backend; nil = an honest "not wired" error
}

// ModelOptions is what the command line sets on the backend.
type ModelOptions struct {
	// Model is --model: the model to ask. Empty keeps the backend's default.
	Model string
	// Tools is the wall from --tools, in sprig's order: the only tools the
	// backend offers the model.
	Tools []string
	// Key is the provider key read from --key-fd; KeySet says one was
	// read. With KeySet, the backend reads no key from the environment.
	Key    string
	KeySet bool
	// Wire is the model wire: claude-code, anthropic or openai. The CLI
	// sets it from --wire, else from SPRIG_BACKEND.
	Wire string
	// BaseURL is --base-url: the server for the anthropic or openai wire.
	// Empty keeps the wire's default.
	BaseURL string
	// NoTools is --tools none: the backend offers the model no tool at
	// all. Tools is then empty.
	NoTools bool
	// System is the bytes of --system-file; SystemSet says a file was
	// read. With SystemSet they replace sprig's own system prompt.
	System    string
	SystemSet bool
	// Extra are the tools of the MCP servers sprig runs itself (the
	// anthropic and openai wires).
	Extra []MCPToolDef
	// MCPConfig is the --mcp-config JSON and MCPServers the server names,
	// for the claude-code wire, where claude runs the servers.
	MCPConfig  string
	MCPServers []string
}

// Wires are the model wires --wire takes, in the order the help names
// them.
var Wires = []string{"claude-code", "anthropic", "openai"}

// usageKeys are the --json usage keys, the names the Messages API and
// claude -p give the four token counts.
var usageKeys = []struct {
	key string
	get func(Usage) int
}{
	{"input_tokens", func(u Usage) int { return u.Fresh }},
	{"output_tokens", func(u Usage) int { return u.Out }},
	{"cache_read_input_tokens", func(u Usage) int { return u.CacheRead }},
	{"cache_creation_input_tokens", func(u Usage) int { return u.CacheWrite }},
}

// usageSum adds the token counts of every assistant turn. A count no turn
// reported (a zero sum) is left out.
func usageSum(history []Message) map[string]int {
	sum := map[string]int{}
	for _, k := range usageKeys {
		n := 0
		for _, m := range history {
			if m.Role == "assistant" {
				n = satAdd(n, k.get(m.Usage))
			}
		}
		if n != 0 {
			sum[k.key] = n
		}
	}
	return sum
}

// Run executes one task and returns a process exit code (0 ok, non-zero fail).
func (c *CLI) Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Nothing a tool started outlives sprig, however it ends (MX-R-8).
	defer KillToolGroups()
	defer onSignalKillToolGroups()()
	fs := flag.NewFlagSet("sprig", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	jsonOut := fs.Bool("json", false, "emit the result as JSON (for scripts and jq)")
	gateOn := fs.Bool("gate", false, "verify each tool call against the envelope, fail-closed")
	maxTurns := fs.Int("max-turns", 20, "give up after this many model turns")
	maxTokens := fs.Int("max-tokens", -1,
		"stop before a model call once the run has used this many tokens (all four usage counts); -1 is no cap")
	gateCmd := fs.String("gate-cmd", DefaultGateCmd(),
		"the out-of-process envelope gate to call when --gate is set")
	sessionDir := fs.String("session-dir", "",
		"write the session tree (JSONL) here — the same shape daisugi's gate writes on path D. Empty = off.")
	sessionID := fs.String("session", "", "session id to write under --session-dir (default: a fresh one)")
	resume := fs.String("resume", "",
		"resume this session id from --session-dir: reopen its file, rebuild history from its head, and append")
	tools := fs.String("tools", strings.Join(AllToolNames, ","),
		"the tools the model may call, a comma list of read, write, edit, bash and spawn, or none")
	systemFile := fs.String("system-file", "",
		"use the bytes of this file as the system prompt, in place of sprig's own")
	modelName := fs.String("model", "",
		"the model to ask (default: haiku on claude -p; SPRIG_MODEL, else claude-haiku-4-5, on the API)")
	keyFd := fs.Int("key-fd", -1,
		"read the provider key from this file descriptor (3 or more) and use no key from the environment")
	wire := fs.String("wire", "",
		"the model wire: claude-code (claude -p), anthropic (the Messages API) or openai (chat completions); default anthropic when SPRIG_BACKEND=api, else claude-code")
	baseURL := fs.String("base-url", "",
		"the server for the anthropic or openai wire (default: ANTHROPIC_BASE_URL, else https://api.anthropic.com; https://api.openai.com/v1 for openai)")
	treeCmd := fs.String("tree-cmd", "",
		"the command the spawn tool runs to start a child (daisugi tree run); spawn is offered only with it")
	spawnKeysFd := fs.Int("spawn-keys-fd", -1,
		"read the resolved agent profiles, with keys, from this file descriptor; the spawn tool passes them on")
	mcpConfig := fs.String("mcp-config", "",
		`the MCP servers whose tools the model may call, as a file or inline JSON {"mcpServers": {name: {command, args, env}}}; stdio servers only`)
	scrubEnv := fs.String("scrub-env", "",
		"a comma list of more variables no tool, gate or backend process gets")
	fs.Usage = func() {
		fmt.Fprintln(stderr, `usage: sprig [flags] "<task>"     (or:  echo "<task>" | sprig -)`)
		fmt.Fprintln(stderr, "a minimal coding-agent harness — the answer goes to stdout, logs to stderr.")
		fmt.Fprintln(stderr, "\nexamples:")
		fmt.Fprintln(stderr, `  sprig "add a test for parse_url"`)
		fmt.Fprintln(stderr, `  sprig --json "list the TODOs" | jq -r .answer`)
		fmt.Fprintln(stderr, "\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, c.Version)
		return 0
	}
	key, keySet := "", false
	if *keyFd >= 0 {
		k, err := ReadKeyFd("--key-fd", *keyFd)
		if err != nil {
			fmt.Fprintln(stderr, "sprig:", err)
			return 2
		}
		if k == "" {
			fmt.Fprintf(stderr, "sprig: --key-fd %d held no key\n", *keyFd)
			return 2
		}
		if KeyHasControl(k) {
			fmt.Fprintf(stderr, "sprig: --key-fd %d held a key with a control character\n", *keyFd)
			return 2
		}
		key, keySet = k, true
	}
	var spawnKeys []byte
	if *spawnKeysFd >= 0 {
		k, err := ReadKeyFd("--spawn-keys-fd", *spawnKeysFd)
		if err != nil {
			fmt.Fprintln(stderr, "sprig:", err)
			return 2
		}
		spawnKeys = []byte(k)
	}
	// No process sprig starts inherits a descriptor above 2, and none
	// gets a key variable or a --scrub-env name.
	CloseInheritedFds()
	SetScrubEnv(strings.Split(*scrubEnv, ","))
	noTools := *tools == "none"
	var wall []string
	if !noTools {
		var err error
		if wall, err = ParseToolWall(*tools); err != nil {
			fmt.Fprintln(stderr, "sprig:", err)
			return 2
		}
	}
	system, systemSet := "", false
	if *systemFile != "" {
		b, err := os.ReadFile(*systemFile)
		if err != nil {
			fmt.Fprintf(stderr, "sprig: --system-file: cannot read %s: %v\n", *systemFile, err)
			return 2
		}
		system, systemSet = string(b), true
	}
	var mcpSpecs []MCPServerSpec
	mcpJSON := ""
	if *mcpConfig != "" {
		var err error
		if mcpSpecs, mcpJSON, err = ParseMCPConfig(*mcpConfig); err != nil {
			fmt.Fprintln(stderr, "sprig:", err)
			return 2
		}
	}
	if slices.Contains(wall, "spawn") {
		if len(strings.Fields(*treeCmd)) == 0 {
			fmt.Fprintln(stderr, "sprig: --tools: spawn needs --tree-cmd")
			return 2
		}
		if *sessionID == "" && *resume == "" {
			fmt.Fprintln(stderr, "sprig: --tools: spawn needs --session, the parent of each child")
			return 2
		}
	}
	w := *wire
	if w == "" {
		w = "claude-code"
		if os.Getenv("SPRIG_BACKEND") == "api" {
			w = "anthropic"
		}
	}
	if !slices.Contains(Wires, w) {
		fmt.Fprintf(stderr, "sprig: --wire: unknown wire %q; the wires are %s\n", w, strings.Join(Wires, ", "))
		return 2
	}
	if *baseURL != "" && w == "claude-code" {
		fmt.Fprintln(stderr, "sprig: --base-url needs --wire anthropic or --wire openai")
		return 2
	}

	if *resume != "" && *sessionDir == "" {
		// Without this, --resume alone silently ran a normal fresh task —
		// the whole session-tree block below is gated on --session-dir, so
		// nothing named the mistake. A user who explicitly asked to resume
		// a session deserves a refusal, not a run that quietly forgot the ask.
		fmt.Fprintln(stderr, "sprig: --resume needs --session-dir too (which session store to resume from)")
		return 1
	}

	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" || task == "-" { // no arg, or explicit '-' → read the task from stdin
		b, _ := io.ReadAll(stdin)
		task = strings.TrimSpace(string(b))
	}
	if task == "" {
		fmt.Fprintln(stderr, "sprig: no task given — pass it as an argument or on stdin (sprig -).")
		fmt.Fprintln(stderr, "try: sprig --help")
		return 1
	}

	newModel := c.NewModel
	if newModel == nil {
		newModel = func(ModelOptions) (Model, error) {
			return nil, fmt.Errorf("no model backend configured")
		}
	}
	opts := ModelOptions{Model: *modelName, Tools: wall, Key: key, KeySet: keySet,
		Wire: w, BaseURL: *baseURL, NoTools: noTools, System: system, SystemSet: systemSet}
	var mcp *MCPSet
	if len(mcpSpecs) > 0 {
		if w == "claude-code" {
			// One decision: claude runs the servers, from the same JSON.
			opts.MCPConfig = mcpJSON
			for _, s := range mcpSpecs {
				opts.MCPServers = append(opts.MCPServers, s.Name)
			}
		} else {
			var err error
			if mcp, err = StartMCP(mcpSpecs, stderr); err != nil {
				fmt.Fprintln(stderr, "sprig:", Redact(err.Error(), key))
				return 1
			}
			defer mcp.Close()
			opts.Extra = mcp.Defs
		}
	}
	model, err := newModel(opts)
	if err != nil {
		fmt.Fprintln(stderr, "sprig:", Redact(err.Error(), key))
		return 1
	}
	// The key the backend holds, from --key-fd or its environment, never
	// reaches an error line.
	if kh, ok := model.(interface{ HeldKey() string }); ok && key == "" {
		key = kh.HeldKey()
	}
	if key != "" {
		model = redactingModel{Model: model, key: key}
	}

	// --gate wires openDaisugi's envelope as an OUT-OF-PROCESS, fail-closed check
	// on every tool call (AgentSpec P1-P4). Gate off = plain-pi AllowAll. If the
	// gate command is missing it fails closed (denies), never silently allows.
	var gate Gate = AllowAll{}
	if *gateOn {
		gate = DaisugiGate{Command: strings.Fields(*gateCmd), Timeout: 10 * time.Second}
	}

	// --session-dir opts into the session tree (path E's counterpart to
	// daisugi's own _log_tree on path D): NoopSessionObserver{} — not a nil
	// SessionObserver — is the off default, so the loop and executor never
	// need a nil check of their own at every call site.
	var obs SessionObserver = NoopSessionObserver{}
	var seedHistory []Message
	if *sessionDir != "" {
		sid := *sessionID
		if *resume != "" {
			sid = *resume
		} else if sid == "" {
			sid = defaultSessionID()
		}
		w, err := openOrNewSessionWriter(*sessionDir, sid, *resume != "")
		if err != nil {
			fmt.Fprintln(stderr, "sprig: session tree:", err)
			return 1
		}
		obs = w
		if *resume != "" {
			entries, err := HeadPath(w.Path())
			if err != nil {
				fmt.Fprintln(stderr, "sprig: session tree: rebuilding history:", err)
				return 1
			}
			seedHistory = HistoryFromEntries(entries)
		}
		if dg, ok := gate.(DaisugiGate); ok {
			dg.SessionID = sid
			gate = dg
		}
		fmt.Fprintf(stderr, "sprig: session %s at %s\n", sid, w.Path())
	}

	toolMap := ToolsFor(wall)
	var spawn *SpawnTool
	if slices.Contains(wall, "spawn") {
		parent := *sessionID
		if *resume != "" {
			parent = *resume
		}
		spawn = &SpawnTool{Command: strings.Fields(*treeCmd), Session: parent, Keys: spawnKeys,
			DefaultTools: HostTools(wall)}
		toolMap["spawn"] = spawn
	}
	if mcp != nil {
		for _, t := range mcp.Tools {
			toolMap[t.Name()] = t
		}
	}
	exec := NewExecutor(toolMap, gate)
	exec.SessionObserver = obs
	agent := &Agent{
		Model: model, Exec: exec, MaxTurns: *maxTurns,
		SessionObserver: obs, SeedHistory: seedHistory,
		MaxTokens: *maxTokens, CapTokens: *maxTokens >= 0,
		NoTools: noTools,
	}
	if spawn != nil {
		agent.ExtraTokens = spawn.BranchTokens
		spawn.OwnUse = func() (int, int) {
			return agent.TokensUsed() - spawn.BranchTokens(), agent.ModelTurns()
		}
	}
	answer, err := agent.Run(task)
	if err != nil {
		fmt.Fprintln(stderr, "sprig:", err)
		return 1
	}

	if *jsonOut {
		name := ""
		if n, ok := model.(interface{ ModelName() string }); ok {
			name = n.ModelName()
		}
		_ = json.NewEncoder(stdout).Encode(map[string]any{"answer": answer, "turns": len(agent.History),
			"model_turns": agent.ModelTurns(), "model": name, "usage": usageSum(agent.History)})
	} else {
		fmt.Fprintln(stdout, answer)
	}
	return 0
}

// openOrNewSessionWriter opens an existing session file for --resume, or
// creates a fresh one otherwise. cwd is best-effort: an error reading it
// (unusual — a deleted working directory) degrades to "" rather than
// blocking the run over a detail the header only records for humans.
func openOrNewSessionWriter(dir, sessionID string, resume bool) (*SessionWriter, error) {
	if resume {
		return OpenSessionWriter(dir, sessionID)
	}
	cwd, _ := os.Getwd()
	return NewSessionWriter(dir, sessionID, cwd)
}

// defaultSessionID mints a fresh session id when --session is not given:
// two newID() draws (8 hex chars each, crypto/rand) for 64 bits of entropy —
// a session id is long-lived (unlike an entry id, minted once per line), so
// it gets a wider draw than the tree's own 8-hex entry ids.
func defaultSessionID() string { return newID() + newID() }
