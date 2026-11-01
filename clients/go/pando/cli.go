package pando

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CLIEnv is what a command line needs from the process around it. The
// package opens no socket; its one outside call is the summarizer, which
// NewSummarizer builds.
type CLIEnv struct {
	Stdin    io.Reader
	Out, Err io.Writer
	// DataHome is the data home (the daisugi rule), where pando/<store>
	// lives, unless --data-dir names another.
	DataHome string
	// Home is $HOME, for the transcripts the importer reads.
	Home string
	// NewSummarizer builds the model behind tend and import. profile is the
	// agent profile's name, or "" for the default.
	NewSummarizer func(profile string) (Summarizer, error)
	// Now is the clock. It defaults to time.Now.
	Now func() time.Time
	// HermesSource builds the source that reads the Hermes profiles; name ""
	// means all of them. nil means this build cannot read Hermes.
	HermesSource func(profile string) (Source, error)
	// Getenv reads the process environment (for DAISUGI_PANDO_PROFILE).
	Getenv func(string) string
}

// Usage is the help of daisugi pando.
const Usage = `Usage: daisugi pando [OPTIONS] COMMAND [ARGS]...

  Pando R2, the conversation memory: a verbatim log, a summary tree and a
  view, in one store on disk. A store is a dir, <data home>/pando/<store>.
  Stores are separate on purpose: a command names one, never two.

Options:
  --store NAME     The store: work (the default) or home, or any a-z0-9- name.
  --data-dir DIR   The data home that holds pando/<store>.
  --json           Print JSON where the command has a list or a record.
  --branch PATH    Read inside a branch of the store (trellis, trellis/ui).
  -q, --quiet      With tend: do not print a line for each node built.
  --help           Show this message and exit.

Commands:
  stores   List the stores.
  status   Show a store: messages, nodes to build, the view's size.
  log      Print the messages, one per line. --day YYYY-MM-DD for one day.
  zoom     Open a node: ID N. N=1 is the message word for word.
  date     Print when message ID was logged, in UTC.
  view     Print the view: the summary lines a turn would see.
  recall   Find where a word was said: QUERY [-k N]. It gives ids, then zoom.
  append   Add a message: TEXT [--kind user|note]. "-" reads standard input.
  tend     Build the summary nodes that are not built yet [--profile NAME]
           [--max N] [--yes] [--dry-run] [-q]. With no --profile it runs Claude
           Code (paid); over 25 nodes it asks for --profile or --yes first.
           --max caps the model calls of the run, a re-ask for a shorter line
           included. Each node built prints one line to stderr; -q hides them.
           --redo-clipped builds again the nodes that hold a clipped copy of
           their source (the model gave no text), and the nodes above them.
           --redo-level N builds again every node at level N and above; with
           both, the run does the union. Both take --profile, --max, --yes
           and --dry-run, and they ask first when they may make over 25 calls.
  import   Bring old chats in: --from claude|codex|web|hermes|all [--dir PATH]
           (all is claude and codex; hermes is read on its own)
           [--web PATH] [--since DATE] [--tools] [--map OLD=NEW] [--dry-run]
           [--propose FILE | --accept FILE] [--tend [--profile NAME]
           [--max N] [--yes]]. --tend builds the summary nodes at once.
           --dir reads Claude Code chats from PATH, not ~/.claude/projects.
           --from hermes logs the Hermes chats now, with no queue: --hermes-profile
           NAME for one profile, --all (or neither) for all; it only reads state.db.
  migrate  Move the old coppice steward log into pando/work [--from DIR].
  serve    Run a read-only MCP server named pando on stdin and stdout.
  context  Print the view with a short note on the tools, as a chat's first
           turn gets it. It prints nothing when the store holds no message.
  hook     A Claude Code hook: reads the event on stdin. SessionStart prints the
           view as context; UserPromptSubmit and Stop log the new messages of the
           transcript. It never fails the chat: errors go to <store>/hook.log.
  profile  Show or set the agent profile that builds nodes in the background:
           [NAME | none]. It is kept in <data home>/pando/profile; the
           DAISUGI_PANDO_PROFILE variable overrides it.
  branches List the branches of the store: long-running agents' parts of the
           tree, each with its state and newest summary.
  close    Close a branch: BRANCH (a path such as trellis or trellis/ui). Its
           messages stay and recall finds them; the view above no longer names it.
  routes   List the session map: which sessions and dirs belong to which branch.
  unroute  Take a session (or dir:PATH) off the session map: ID. Its next lines
           go to the store again; lines already logged stay where they are.
  follow   Keep the store live: log new Claude Code and Hermes messages as they
           come and build the summary nodes in the background [--profile NAME]
           [--interval SECONDS] [--once] [--claude-dir DIR] [--hermes-profile NAME | --all].
           With no profile (flag or pando.profile) it only logs, and says once that
           the nodes wait. A model outage delays a node and never clips it.
`

type cliFlags struct {
	store, dataDir, day, kind, profile, from, since, dir string
	web, propose, accept, child, migFrom                 string
	branch, session                                      string
	offset                                               int64
	size, k, max                                         int
	redoLevel                                            int
	asJSON, dry, tools, help, tend, yes, redo, hasLevel  bool
	quiet                                                bool
	once, all                                            bool
	hermesProfile, claudeDir                             string
	interval                                             int
	maps                                                 []string
	summ                                                 Summarizer
	words                                                []string
}

func usageErr(env CLIEnv, format string, a ...any) int {
	fmt.Fprintf(env.Err, format+"\n", a...)
	fmt.Fprintln(env.Err, "Try 'daisugi pando --help' for help.")
	return 2
}

// Run runs daisugi pando with the words after "pando". It returns the exit
// code: 0 done, 1 a store or id that is not there or a failed step, 2 usage.
func Run(argv []string, env CLIEnv) int {
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	f := cliFlags{store: DefaultStore, k: 8}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		val := func() (string, bool) {
			if i+1 >= len(argv) {
				return "", false
			}
			i++
			return argv[i], true
		}
		need := func(dst *string) bool {
			v, ok := val()
			if !ok {
				fmt.Fprintf(env.Err, "%s needs a value\n", a)
				return false
			}
			*dst = v
			return true
		}
		needInt := func() (int64, bool) {
			v, ok := val()
			if !ok {
				fmt.Fprintf(env.Err, "%s needs a number\n", a)
				return 0, false
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				fmt.Fprintf(env.Err, "%s needs a number, not %q\n", a, v)
				return 0, false
			}
			return n, true
		}
		ok := true
		switch a {
		case "--store":
			ok = need(&f.store)
		case "--data-dir":
			ok = need(&f.dataDir)
		case "--day":
			ok = need(&f.day)
		case "--kind":
			ok = need(&f.kind)
		case "--profile":
			ok = need(&f.profile)
		case "--from":
			ok = need(&f.from)
			f.migFrom = f.from
		case "--since":
			ok = need(&f.since)
		case "--web":
			ok = need(&f.web)
		case "--propose":
			ok = need(&f.propose)
		case "--accept":
			ok = need(&f.accept)
		case "--child":
			ok = need(&f.child)
		case "--branch":
			ok = need(&f.branch)
		case "--session":
			ok = need(&f.session)
		case "--hermes-profile":
			ok = need(&f.hermesProfile)
		case "--claude-dir":
			ok = need(&f.claudeDir)
		case "--interval":
			var n int64
			if n, ok = needInt(); ok {
				f.interval = int(n)
			}
		case "--once":
			f.once = true
		case "--all":
			f.all = true
		case "--dir":
			ok = need(&f.dir)
		case "--map":
			var m string
			if ok = need(&m); ok {
				f.maps = append(f.maps, m)
			}
		case "--offset":
			var n int64
			if n, ok = needInt(); ok {
				f.offset = n
			}
		case "--size":
			var n int64
			if n, ok = needInt(); ok {
				f.size = int(n)
			}
		case "-k":
			var n int64
			if n, ok = needInt(); ok {
				f.k = int(n)
			}
		case "--max":
			var n int64
			if n, ok = needInt(); ok {
				f.max = int(n)
			}
		case "--json":
			f.asJSON = true
		case "--dry-run":
			f.dry = true
		case "--tools":
			f.tools = true
		case "--tend":
			f.tend = true
		case "--redo-clipped":
			f.redo = true
		case "--redo-level":
			var n int64
			if n, ok = needInt(); ok {
				f.redoLevel, f.hasLevel = int(n), true
			}
		case "--yes", "-y":
			f.yes = true
		case "--quiet", "-q":
			f.quiet = true
		case "--help", "-h":
			f.help = true
		default:
			if strings.HasPrefix(a, "--") {
				return usageErr(env, "No such option: %s", a)
			}
			f.words = append(f.words, a)
		}
		if !ok {
			return 2
		}
	}
	if f.help || len(f.words) == 0 {
		fmt.Fprint(env.Out, Usage)
		if f.help {
			return 0
		}
		return 2
	}
	verb, args := f.words[0], f.words[1:]
	arity := map[string][2]int{"stores": {0, 0}, "status": {0, 0}, "log": {0, 0}, "zoom": {2, 2}, "date": {1, 1},
		"view": {0, 0}, "recall": {1, 1}, "append": {1, -1}, "tend": {0, 0}, "import": {0, 0}, "migrate": {0, 0}, "serve": {0, 0}, "hook": {0, 0}, "follow": {0, 0}, "profile": {0, 1}, "context": {0, 0}, "branches": {0, 0}, "close": {1, 1}, "routes": {0, 0}, "unroute": {1, 1}}
	ar, known := arity[verb]
	if !known {
		return usageErr(env, "No such command '%s'.", verb)
	}
	if len(args) < ar[0] || (ar[1] >= 0 && len(args) > ar[1]) {
		return usageErr(env, "daisugi pando %s takes %d argument(s).", verb, ar[0])
	}
	if f.all && (verb != "follow" && !(verb == "import" && f.from == "hermes")) {
		return usageErr(env, "--all goes with follow or import --from hermes")
	}
	if f.all && f.hermesProfile != "" {
		return usageErr(env, "--all and --hermes-profile exclude each other")
	}
	if f.redo && verb != "tend" {
		return usageErr(env, "--redo-clipped goes with tend")
	}
	if f.hasLevel && verb != "tend" {
		return usageErr(env, "--redo-level goes with tend")
	}
	data := f.dataDir
	if data == "" {
		data = env.DataHome
	}
	if verb == "stores" {
		if env.Getenv(BranchVar) != "" || SkipOn(env.Getenv) {
			fmt.Fprintln(env.Err, markedRefusal(env.Getenv(BranchVar), verb))
			return 1
		}
		return cliStores(env, data, f)
	}
	dir, err := StoreDir(data, f.store)
	if err != nil {
		if verb == "hook" {
			return 0 // a hook never fails the chat
		}
		return usageErr(env, "%v", err)
	}
	if verb == "profile" {
		return cliProfile(env, data, args)
	}
	branchMark := strings.TrimSpace(env.Getenv(BranchVar))
	if verb == "hook" {
		// The event is read first in every case: the parent writes it to our
		// standard input and must not meet a closed pipe.
		ev := ReadHookEvent(bufio.NewReader(env.Stdin))
		if SkipOn(env.Getenv) {
			// Not the owner's chat: no view, nothing logged. The session goes
			// on the skip list, so the follow service and the importer leave
			// its transcript alone (it lives in a project dir, where the
			// owner's own chats live too).
			// A spawner that names its own list (DAISUGI_PANDO_SKIPLIST, as
			// coppice does under a --data-dir of its own) is listed there too.
			list := SkipListPath(dir)
			SkipSession(list, ev)
			if named := env.Getenv(SkipListVar); named != "" && named != list {
				SkipSession(named, ev)
			}
			return 0
		}
		if branchMark != "" {
			// A long-running agent: its view is its branch, and what it says
			// goes into its branch, never into the store above.
			fmt.Fprint(env.Out, RunBranchHook(context.Background(), dir, branchMark, ev, SkipDirs(dir, env.Home, env.Getenv), env.Now))
			return 0
		}
		// The owner's own chat claims its session for the store at its
		// start, before any branch can map it (G5).
		if ev.Event == "SessionStart" {
			_ = ClaimRoot(dir, ev.Cwd, ev.SessionID, ev.TranscriptPath)
		}
		fmt.Fprint(env.Out, RunHook(context.Background(), dir, ev, SkipDirs(dir, env.Home, env.Getenv), env.Now))
		return 0
	}
	sc := ScopeOf(dir, env.Getenv)
	if verb == "serve" && sc.Closed {
		// A worker with no branch, or a branch that cannot be read: the
		// server lists no tool. It does not open the store, so a store that
		// is missing or large costs the process nothing.
		if err := ServeScope(context.Background(), sc, env.Stdin, env.Out); err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		return 0
	}
	if branchMark != "" || SkipOn(env.Getenv) {
		// A process in a branch reads its branch and below, and writes
		// nothing by hand: its hooks and the follow service log it.
		switch verb {
		case "stores", "migrate", "append", "tend", "import", "follow", "profile", "close", "routes", "unroute":
			fmt.Fprintln(env.Err, markedRefusal(branchMark, verb))
			return 1
		}
		if verb == "context" && (sc.Closed || sc.Worker) {
			return 0 // no view for a worker, and never the store above
		}
		if sc.Closed {
			fmt.Fprintln(env.Err, sc.Why)
			return 1
		}
		if verb == "context" && ValidSessionID(f.session) && !sc.Worker {
			// The Hermes plugin names its session: the follow service then
			// logs its rows into this branch. A process that cannot write
			// the map (a fenced pane) leaves it to coppice.
			_ = AddRoute(dir, sc.Branch, "hermes:"+f.session)
		}
	}
	if verb == "routes" || verb == "unroute" {
		return cliRoutes(env, dir, f, verb, args)
	}
	if verb != "migrate" && verb != "append" && verb != "import" && verb != "follow" && verb != "close" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			fmt.Fprintf(env.Err, "no store %s at %s\n", f.store, dir)
			return 1
		}
	}
	if verb == "close" {
		if err := CloseBranch(dir, args[0], env.Now); err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		fmt.Fprintf(env.Out, "closed: %s\n", args[0])
		return 0
	}
	switch verb {
	case "migrate":
		return cliMigrate(env, data, f)
	case "append":
		return cliAppend(env, dir, f, args)
	case "tend":
		if f.redo || f.hasLevel {
			return cliRedo(env, dir, f)
		}
		return cliTend(env, dir, f)
	case "import":
		if f.from == "hermes" {
			return cliImportHermes(env, dir, f)
		}
		return cliImport(env, data, dir, f)
	case "follow":
		return cliFollow(env, dir, f)
	}
	root, err := sc.Below(f.branch)
	if err != nil {
		if f.branch != "" {
			return usageErr(env, "%v", err)
		}
		fmt.Fprintln(env.Err, err)
		return 1
	}
	if f.child != "" {
		if root, err = ChildDir(root, f.child); err != nil {
			return usageErr(env, "%v", err)
		}
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			fmt.Fprintf(env.Err, "no child %s in store %s\n", f.child, f.store)
			return 1
		}
	}
	if verb == "branches" {
		return cliBranches(env, root, f)
	}
	if verb == "context" && f.child == "" {
		// The view text needs the view, the tree and the last messages only.
		text, err := ViewContextOf(root)
		if err != nil {
			if !hasMain(root) {
				fmt.Fprint(env.Out, BranchLines(root))
				return 0
			}
			fmt.Fprintln(env.Err, err)
			return 1
		}
		fmt.Fprint(env.Out, text)
		return 0
	}
	if verb == "serve" {
		sc.Dir = root
		if err := ServeScope(context.Background(), sc, env.Stdin, env.Out); err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		return 0
	}
	c, err := OpenReadOnly(root)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	if n := c.Notice(); n != "" {
		fmt.Fprintln(env.Err, "note: "+n)
	}
	return cliRead(env, c, root, f, verb, args)
}

func printJSON(env CLIEnv, v any) int {
	line, err := encodeLine(v)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	env.Out.Write(line)
	return 0
}

func cliStores(env CLIEnv, data string, f cliFlags) int {
	names, err := ListStores(data)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	if f.asJSON {
		if names == nil {
			names = []string{}
		}
		return printJSON(env, names)
	}
	for _, n := range names {
		fmt.Fprintln(env.Out, n)
	}
	return 0
}

func cliRead(env CLIEnv, c *Chat, dir string, f cliFlags, verb string, args []string) int {
	switch verb {
	case "status":
		rec := map[string]any{"store": f.store, "path": dir, "messages": c.Count(), "pending": c.Pending(),
			"not_in_view": c.NotInView(), "view_bytes": c.ViewSize(), "clipped": c.ClippedCount(), "notice": c.Notice()}
		if f.child != "" {
			rec["child"] = f.child
		}
		if f.asJSON {
			return printJSON(env, rec)
		}
		fmt.Fprintf(env.Out, "store: %s\npath: %s\nmessages: %d\npending nodes: %d\nnot in the view yet: %d\nview bytes: %d\nclipped nodes: %d\n",
			f.store, dir, c.Count(), c.Pending(), c.NotInView(), c.ViewSize(), c.ClippedCount())
		if f.child == "" {
			fmt.Fprintf(env.Out, "children: %d\n", len(ListChildren(dir)))
			if n := len(BranchTree(dir)); n > 0 {
				fmt.Fprintf(env.Out, "branches: %d\n", n)
			}
		}
		if n := c.Notice(); n != "" {
			fmt.Fprintf(env.Out, "notice: %s\n", n)
		}
	case "log":
		var msgs []Message
		var err error
		if f.day != "" {
			if msgs, err = c.Day(f.day); err != nil {
				fmt.Fprintln(env.Err, err)
				return 1
			}
		} else {
			for i := int64(0); i < c.Count(); i++ {
				m, err := c.Message(i)
				if err != nil {
					fmt.Fprintln(env.Err, err)
					return 1
				}
				msgs = append(msgs, m)
			}
		}
		for _, m := range msgs {
			if f.asJSON {
				if code := printJSON(env, m); code != 0 {
					return code
				}
				continue
			}
			fmt.Fprintf(env.Out, "%d\t%s\t%s\t%s\n", m.I, m.Kind, m.Date, flat(m.Text))
		}
	case "view":
		ents := c.ViewEntries()
		if f.asJSON {
			type row struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			}
			rows := make([]row, len(ents))
			for i, e := range ents {
				rows[i] = row{e.ID.Name(), e.Text}
			}
			return printJSON(env, rows)
		}
		for _, e := range ents {
			fmt.Fprintf(env.Out, "%s\t%s\n", e.ID.Name(), e.Text)
		}
		// The branches below, as context, the hook and the view tool give them.
		if f.child == "" {
			fmt.Fprint(env.Out, BranchLines(dir))
		}
	case "zoom":
		id, e1 := strconv.ParseInt(args[0], 10, 64)
		n, e2 := strconv.ParseInt(args[1], 10, 64)
		if e1 != nil || e2 != nil {
			return usageErr(env, "zoom needs two numbers, ID and N, not %q %q", args[0], args[1])
		}
		text, err := c.ZoomPage(id, n, f.offset, f.size)
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		if f.asJSON {
			return printJSON(env, map[string]string{"text": text})
		}
		fmt.Fprint(env.Out, text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Fprintln(env.Out)
		}
	case "date":
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return usageErr(env, "date needs a message id, not %q", args[0])
		}
		d, err := c.Date(id)
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		if f.asJSON {
			return printJSON(env, map[string]string{"date": d})
		}
		fmt.Fprintln(env.Out, d)
	case "recall":
		hits := c.Recall(args[0], f.k)
		if f.child == "" {
			var err error
			if hits, err = RecallTree(dir, args[0], f.k); err != nil {
				fmt.Fprintln(env.Err, err)
				return 1
			}
		}
		if f.asJSON {
			type row struct {
				ID    string  `json:"id"`
				Score float64 `json:"score"`
				Line  string  `json:"line"`
			}
			rows := make([]row, len(hits))
			for i, h := range hits {
				rows[i] = row{h.ID, h.Score, h.Line}
			}
			return printJSON(env, rows)
		}
		for _, h := range hits {
			fmt.Fprintln(env.Out, FormatHit(h))
		}
	case "context":
		if !SkipOn(env.Getenv) {
			fmt.Fprint(env.Out, c.ViewContext())
		}
	}
	return 0
}

func cliAppend(env CLIEnv, dir string, f cliFlags, args []string) int {
	kind := f.kind
	if kind == "" {
		kind = KindUser
	}
	if kind != KindUser && kind != KindNote {
		return usageErr(env, "--kind takes user or note, not %q", kind)
	}
	text := strings.Join(args, " ")
	if text == "-" {
		b, err := io.ReadAll(env.Stdin)
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		text = strings.TrimRight(string(b), "\n")
	}
	if strings.TrimSpace(text) == "" {
		return usageErr(env, "append needs some text")
	}
	c, err := Open(context.Background(), Config{Dir: dir, Now: env.Now})
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	defer c.Close()
	if n := c.Notice(); n != "" {
		fmt.Fprintln(env.Err, "note: "+n)
	}
	ids, err := c.Append(context.Background(), kind, text)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	if f.asJSON {
		return printJSON(env, ids)
	}
	for _, id := range ids {
		fmt.Fprintln(env.Out, id)
	}
	return 0
}

// PaidTendLimit is how many nodes tend builds with the default model, Claude
// Code, before it asks for --profile or --yes.
const PaidTendLimit = 25

// paidDefault is true when profile names the default model, which runs on
// Claude Code and is paid per call.
func paidDefault(profile string) bool { return profile == "" || profile == "default" }

// paidTendRefusal is the text that stops a tend that would make more than
// PaidTendLimit paid calls, or "" when it may go on. max caps the calls (0 is
// no cap), so a run bounded by --max is its own limit.
func paidTendRefusal(pending, max int, f cliFlags) string {
	calls := pending
	if max > 0 && max < calls {
		calls = max
	}
	if f.yes || !paidDefault(f.profile) || calls <= PaidTendLimit {
		return ""
	}
	return fmt.Sprintf("The default model is Claude Code (paid), and this run makes at most %d model calls. Nothing was built.\n", calls) +
		"Run again with --profile NAME to use an agent profile, or --yes to accept the default.\n"
}

// pendingAll counts the nodes to build in a store and its children.
func pendingAll(dir string) (int, error) {
	roots := []string{dir}
	for _, ss := range SubStores(dir) {
		roots = append(roots, ss.Dir)
	}
	n := 0
	for _, r := range roots {
		c, err := OpenReadOnly(r)
		if err != nil {
			return 0, err
		}
		n += c.Pending()
	}
	return n, nil
}

func cliTend(env CLIEnv, dir string, f cliFlags) int {
	total, err := pendingAll(dir)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	fmt.Fprintf(env.Out, "nodes to build: %d\n", total)
	if f.dry {
		subs := append([]SubStore{{Dir: dir}}, SubStores(dir)...)
		for _, ss := range subs {
			c, err := OpenReadOnly(ss.Dir)
			if err != nil {
				fmt.Fprintln(env.Err, err)
				return 1
			}
			label := ss.Rel
			fmt.Fprintf(env.Out, "%spending: %d\n%snot in the view yet: %d\n", label, c.Pending(), label, c.NotInView())
			for _, id := range c.PendingIDs() {
				fmt.Fprintf(env.Out, "%s%s\n", label, id.Name())
			}
		}
		fmt.Fprintln(env.Out, "nothing was changed")
		return 0
	}
	if why := paidTendRefusal(total, f.max, f); why != "" {
		fmt.Fprint(env.Err, why)
		return 1
	}
	if env.NewSummarizer == nil {
		fmt.Fprintln(env.Err, "no model is set up for tend")
		return 1
	}
	s, err := env.NewSummarizer(f.profile)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	c, err := Open(context.Background(), Config{Dir: dir, Now: env.Now})
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	defer c.Close()
	if n := c.Notice(); n != "" {
		fmt.Fprintln(env.Err, "note: "+n)
	}
	capCalls(s, f.max)
	run := &tendRun{stopOnClips: true}
	run.progressFor(env, f)
	err = tendCalls(context.Background(), c, s, f.max, run)
	if err == nil && (f.max == 0 || run.spent < f.max) {
		rest := 0
		if f.max > 0 {
			rest = f.max - run.spent
		}
		err = tendChildren(context.Background(), dir, s, rest, env.Now, run)
	}
	return finishTend(env, run, c.Pending(), err, "")
}

// ClippedWhy is the one line tend says when some nodes fell back to a
// clipped copy of the source.
const ClippedWhy = "The model returned no text for the clipped nodes. If it is a thinking model, " +
	"raise max_tokens in its profile or use a non-thinking model. Build them again with: " +
	"daisugi pando tend --redo-clipped"

// finishTend prints the end of a tend run: the counts, and why when a node
// fell back to clipped text. It returns the exit code. after is a line to
// add when the run stopped on an error.
func finishTend(env CLIEnv, run *tendRun, pending int, err error, after string) int {
	fmt.Fprintf(env.Out, "built: %d, fell back to clipped: %d\npending: %d\n", run.built, run.clipped, pending)
	if run.clipped > 0 && !errors.Is(err, ErrAllClipped) {
		fmt.Fprintln(env.Out, ClippedWhy)
	}
	if err != nil {
		fmt.Fprintf(env.Err, "tend stopped: %v\n", err)
		if errors.Is(err, ErrAllClipped) {
			fmt.Fprintln(env.Err, "The nodes built so far hold clipped text. After you fix the model, run: daisugi pando tend --redo-clipped")
		}
		if after != "" {
			fmt.Fprint(env.Err, after)
		}
		return 1
	}
	return 0
}

// cliRedo is tend --redo-clipped and tend --redo-level N: build again the
// nodes that hold a clipped fallback and the nodes above them, and the nodes
// at level N and above, with the same budget and the same refusal of a paid
// run as tend. The refusal counts the model calls the plan can make.
func cliRedo(env CLIEnv, dir string, f cliFlags) int {
	set := RedoSet{Clipped: f.redo, FromLevel: f.redoLevel, HasLevel: f.hasLevel}
	subs := append([]SubStore{{Dir: dir}}, SubStores(dir)...)
	total, calls := 0, 0
	var lines []string
	for _, ss := range subs {
		c, err := OpenReadOnly(ss.Dir)
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		label := ss.Rel
		c.redoModel = f.profile
		ids, n := c.RedoPlan(set)
		calls += n
		for _, id := range ids {
			total++
			lines = append(lines, label+id.Name())
		}
	}
	fmt.Fprintf(env.Out, "nodes to build again: %d\nmodel calls: at most %d\n", total, calls)
	if f.dry {
		for _, l := range lines {
			fmt.Fprintln(env.Out, l)
		}
		fmt.Fprintln(env.Out, "nothing was changed")
		return 0
	}
	if total == 0 {
		for _, ss := range subs {
			redoEnd(ss.Dir, set, f.profile)
		}
		return 0
	}
	if why := paidTendRefusal(calls, f.max, f); why != "" {
		fmt.Fprint(env.Err, why)
		return 1
	}
	if env.NewSummarizer == nil {
		fmt.Fprintln(env.Err, "no model is set up for tend")
		return 1
	}
	s, err := env.NewSummarizer(f.profile)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	c, err := Open(context.Background(), Config{Dir: dir, Now: env.Now})
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	defer c.Close()
	if n := c.Notice(); n != "" {
		fmt.Fprintln(env.Err, "note: "+n)
	}
	capCalls(s, f.max)
	run := &tendRun{stopOnClips: true, model: f.profile}
	run.progressFor(env, f)
	ctx := context.Background()
	err = redoCalls(ctx, c, s, set, f.max, run)
	if err == nil && (f.max == 0 || run.spent < f.max) {
		rest := 0
		if f.max > 0 {
			rest = f.max - run.spent
		}
		err = redoChildren(ctx, dir, s, set, rest, func(d string) (*Chat, error) {
			return Open(ctx, Config{Dir: d, Now: env.Now})
		}, run)
	}
	if err == nil && (f.max == 0 || run.spent < f.max) {
		// Every store was gone through: a next run starts again.
		for _, ss := range subs {
			if !slices.Contains(run.locked, ss.Dir) {
				redoEnd(ss.Dir, set, f.profile)
			}
		}
	}
	fmt.Fprintf(env.Out, "built again: %d, fell back to clipped: %d\n", run.built, run.clipped)
	if run.clipped > 0 && !errors.Is(err, ErrAllClipped) {
		fmt.Fprintln(env.Out, ClippedWhy)
	}
	if err != nil {
		fmt.Fprintf(env.Err, "tend stopped: %v\n", err)
		return 1
	}
	return 0
}

func cliMigrate(env CLIEnv, data string, f cliFlags) int {
	from := f.migFrom
	if from == "" {
		from = pathJoin(data, "foreman")
	}
	rep, err := Migrate(from, data, f.store, env.Now())
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	if rep.Already {
		fmt.Fprintf(env.Out, "already moved: %s\n", rep.MovedTo)
		return 0
	}
	if f.asJSON {
		return printJSON(env, rep)
	}
	fmt.Fprintf(env.Out, "copied: %s\nmessages: %d\nmoved: %s\n", strings.Join(rep.Copied, ", "), rep.Messages, rep.MovedTo)
	if len(rep.Kept) > 0 {
		fmt.Fprintf(env.Out, "kept in the moved dir for coppice: %s\n", strings.Join(rep.Kept, ", "))
	}
	return 0
}

func parseMaps(env CLIEnv, words []string) ([]PathMap, bool) {
	var out []PathMap
	for _, w := range words {
		m, err := ParseMap(w)
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

func cliImport(env CLIEnv, data, dir string, f cliFlags) int {
	from := f.from
	if from == "" {
		from = "all"
	}
	if from != "claude" && from != "codex" && from != "web" && from != "all" {
		return usageErr(env, "--from needs claude, codex, web or all, not %q", from)
	}
	maps, ok := parseMaps(env, f.maps)
	if !ok {
		return 2
	}
	o := ImportOpts{From: from, Tools: f.tools, Map: maps, WebZip: f.web}
	if f.dir != "" {
		if f.from != "" && f.from != "claude" {
			return usageErr(env, "--dir holds Claude Code chats: use it with --from claude, not --from %s", f.from)
		}
		if f.propose != "" || f.accept != "" {
			return usageErr(env, "--dir does not go with --propose or --accept")
		}
		if st, err := os.Stat(f.dir); err != nil || !st.IsDir() {
			fmt.Fprintf(env.Err, "no folder %s\n", f.dir)
			return 1
		}
		// Absolute, so a queued session names the same file from any directory.
		abs, err := filepath.Abs(f.dir)
		if err != nil {
			abs = f.dir
		}
		o.From, o.Dir = "claude", abs
	}
	if f.since != "" {
		t, err := time.Parse("2006-01-02", f.since)
		if err != nil {
			return usageErr(env, "--since needs a date like 2026-03-01, not %q", f.since)
		}
		o.Since = t.UTC()
	}
	if f.propose != "" && f.accept != "" {
		return usageErr(env, "give --propose or --accept, not both")
	}
	if f.tend && !f.dry && f.propose == "" {
		// The model is chosen before anything is written, so a store is
		// never left half imported for want of one.
		if env.NewSummarizer == nil {
			fmt.Fprintln(env.Err, "no model is set up for tend")
			return 1
		}
		s, err := env.NewSummarizer(f.profile)
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		f.summ = s
	}
	if from == "web" || f.propose != "" || f.accept != "" {
		return cliImportWeb(env, data, f, o)
	}
	return cliImportLocal(env, dir, f, o)
}

func hasActOf(dir string) func(string) bool {
	if c, err := OpenReadOnly(dir); err == nil {
		return c.HasAct
	}
	return nil
}

func cliImportLocal(env CLIEnv, dir string, f cliFlags, o ImportOpts) int {
	if o.From == "all" {
		o.From = "all"
	}
	o.Skip = []string{dir}
	st, err := ReadImportState(dir)
	if err != nil {
		fmt.Fprintf(env.Err, "cannot read the import state in %s: %v\n", dir, err)
		return 1
	}
	ss := Scan(env.Home, o, st, hasActOf(dir))
	fmt.Fprint(env.Out, Describe(ss))
	if f.dry {
		fmt.Fprintln(env.Out, "nothing was changed")
		return 0
	}
	if len(ss) == 0 {
		fmt.Fprintln(env.Out, "nothing to import")
		return 0
	}
	return acceptAndFeed(env, dir, f, o, ss)
}

// acceptAndFeed queues the sessions and logs them. When another process
// holds the store, they stay queued for it.
func acceptAndFeed(env CLIEnv, dir string, f cliFlags, o ImportOpts, ss []ImportSession) int {
	if err := Accept(dir, o, ss); err != nil {
		fmt.Fprintf(env.Err, "cannot write the import state in %s: %v\n", dir, err)
		return 1
	}
	c, err := Open(context.Background(), Config{Dir: dir, Now: env.Now})
	if errors.Is(err, ErrLocked) {
		fmt.Fprintln(env.Out, "queued: the process that has the store open logs them, one message at a time")
		return 0
	}
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	defer c.Close()
	im := &Importer{Dir: dir, Skip: o.Skip}
	n := 0
	for {
		fed, err := im.Step(context.Background(), c)
		if err != nil {
			fmt.Fprintf(env.Err, "import stopped: %v\n", err)
			return 1
		}
		if !fed {
			break
		}
		n++
	}
	if f.summ == nil {
		fmt.Fprintf(env.Out, "logged: %d messages\nnodes to build: %d. Run: daisugi pando tend\n", n, c.Pending())
		return 0
	}
	fmt.Fprintf(env.Out, "logged: %d messages\n", n)
	if why := paidTendRefusal(c.Pending(), f.max, f); why != "" {
		fmt.Fprintf(env.Err, "nodes to build: %d\n%sThe messages are logged. Run: daisugi pando tend\n", c.Pending(), why)
		return 1
	}
	capCalls(f.summ, f.max)
	run := &tendRun{stopOnClips: true}
	run.progressFor(env, f)
	err = tendCalls(context.Background(), c, f.summ, f.max, run)
	return finishTend(env, run, c.Pending(), err, "the rest stays pending. Run: daisugi pando tend\n")
}

func cliImportWeb(env CLIEnv, data string, f cliFlags, o ImportOpts) int {
	if f.web == "" {
		return usageErr(env, "--from web needs --web PATH, the export zip or its conversations.json")
	}
	storeHas := func(store string) func(string) bool {
		d, err := StoreDir(data, store)
		if err != nil {
			return nil
		}
		return hasActOf(d)
	}
	if f.accept == "" {
		ss, err := ScanWeb(f.web, o, storeHas(f.store))
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		if f.propose != "" {
			w, err := os.Create(f.propose)
			if err != nil {
				fmt.Fprintln(env.Err, err)
				return 1
			}
			err = WriteProposal(w, ss)
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				fmt.Fprintln(env.Err, err)
				return 1
			}
			fmt.Fprintf(env.Out, "wrote %s: %d chats. Edit the store of each line, then run import --accept. No store was changed.\n", f.propose, len(ss))
			return 0
		}
		all, _ := Totals(ss)
		fmt.Fprintf(env.Out, "web chats: %d (about %s)\nnothing was changed\n", len(ss), plural(all.Messages, "message"))
		return 0
	}
	pf, err := os.Open(f.accept)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	choices, err := ReadProposal(pf)
	pf.Close()
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 2
	}
	all, err := ScanWeb(f.web, o, nil)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	byKey := map[string]ImportSession{}
	for _, s := range all {
		byKey[s.Key] = s
	}
	perStore := map[string][]ImportSession{}
	var order []string
	for _, ch := range choices {
		s, ok := byKey[ch.Key]
		if !ok {
			fmt.Fprintf(env.Err, "the export has no chat %s\n", ch.Key)
			return 1
		}
		if ch.Store == "skip" {
			continue
		}
		if _, seen := perStore[ch.Store]; !seen {
			order = append(order, ch.Store)
		}
		perStore[ch.Store] = append(perStore[ch.Store], s)
	}
	for _, store := range order {
		d, err := StoreDir(data, store)
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 2
		}
		fmt.Fprintf(env.Out, "store %s: %d chats\n", store, len(perStore[store]))
		o2 := o
		o2.From = SrcWeb
		if code := acceptAndFeed(env, d, f, o2, perStore[store]); code != 0 {
			return code
		}
	}
	if len(order) == 0 {
		fmt.Fprintln(env.Out, "every line said skip; nothing was changed")
	}
	return 0
}

// capCalls makes --max cap every model call of the run, a re-ask for a
// shorter line included, when the summarizer can be capped.
func capCalls(s Summarizer, max int) {
	if b, ok := s.(interface{ SetBudget(int) }); ok {
		b.SetBudget(max)
	}
}

// progressFor makes run print one line to stderr for each node it writes,
// unless -q.
func (run *tendRun) progressFor(env CLIEnv, f cliFlags) {
	if !f.quiet {
		run.progress = progressTo(env.Err)
	}
}

// markedRefusal is what a process with a marker hears for a verb that is the
// owner's: a process in a branch, or a worker (F6).
func markedRefusal(branch, verb string) string {
	if strings.TrimSpace(branch) == "" {
		return fmt.Sprintf("this process is a worker of the memory; %s is for the owner's own shell", verb)
	}
	return fmt.Sprintf("this process works in a branch of the memory (%s); %s is for the owner's own shell", branch, verb)
}

// cliRoutes is `routes` and `unroute`: the owner reads and fixes the session
// map of a store (F2).
func cliRoutes(env CLIEnv, dir string, f cliFlags, verb string, args []string) int {
	if verb == "unroute" {
		found, err := RemoveRoute(dir, args[0])
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		if !found {
			fmt.Fprintf(env.Err, "no route for %s\n", args[0])
			return 1
		}
		fmt.Fprintf(env.Out, "unrouted: %s\n", args[0])
		return 0
	}
	rows := RouteRows(dir)
	if f.asJSON {
		type row struct {
			ID     string `json:"id"`
			Branch string `json:"branch"`
		}
		out := make([]row, len(rows))
		for i, r := range rows {
			out[i] = row{r[0], r[1]}
		}
		return printJSON(env, out)
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.Out, "no route")
		return 0
	}
	for _, r := range rows {
		fmt.Fprintf(env.Out, "%s\t%s\n", r[0], r[1])
	}
	return 0
}
