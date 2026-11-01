package steward

import (
	"daisugi-verify/pando"
	"encoding/json"
	"fmt"
	"github.com/opendaisugi/coppice/internal/datahome"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Usage is the help for coppice steward. The command only reads, except import, which writes import.json only; it never
// takes the chat's lock.
var Usage = strings.ReplaceAll(usageText, "{dir}", DirName)

const usageText = `coppice steward: read the steward's Pando R2 log. Only import changes anything.

  coppice steward log [--day YYYY-MM-DD] [--json]   the messages, one per line
  coppice steward view                              the view a turn would see
  coppice steward zoom ID+N [--offset B] [--size B] open a node; N=1 is the message;
                                                    a long node comes in pages
  coppice steward date ID                           when message ID was logged
  coppice steward stats                             the turns and the cache read share
  coppice steward status [--json]                   the turn profiles and the state of each:
                                                    ready, cooling until T, or failing
  coppice steward queue [--json]                    the jobs that wait for a machine, and the
                                                    pool entries: free, busy or cooling
  coppice steward queue add --project P --brief TEXT --profile POOL [--model M]
                                                    add a background job; it runs on a pool
                                                    entry that has been idle (pools.toml)
  coppice steward import [--from claude|codex|all] [--since DATE] [--tools]
                         [--dry-run | --pause | --resume | --status [--json]]
                                                    bring old Claude Code and Codex chats
                                                    into the steward, only if you ask;
                                                    --dry-run counts them and changes nothing

  --data-dir DIR   the steward dir itself. Without it, coppice --data-dir X
                   reads X/{dir} (for coppice's default data dir, the
                   steward dir beside it, as below); with neither, $OPENDAISUGI_HOME/{dir},
                   else $XDG_DATA_HOME/opendaisugi/{dir} when it is set
                   (an old ~/.opendaisugi/coppice that holds data keeps
                   ~/.opendaisugi/{dir}), else ~/.opendaisugi/{dir}.`

// Main runs coppice steward with the words after "steward". coppiceDataDir
// is coppice's own --data-dir, or "". It returns the exit code: 0 done, 1 a
// user error.
func Main(argv []string, coppiceDataDir string, out, errw io.Writer) int {
	dir, day, asJSON, store := "", "", false, pando.DefaultStore
	imp := pando.ImportFlags{From: "all"}
	var offset int64
	size := 0
	var words []string
	var qreq QueueRequest
	for k := 0; k < len(argv); k++ {
		switch a := argv[k]; a {
		case "--offset", "--size":
			if k+1 >= len(argv) {
				fmt.Fprintf(errw, "%s needs a value\n", a)
				return 1
			}
			v, err := strconv.ParseInt(argv[k+1], 10, 64)
			if err != nil || v < 0 {
				fmt.Fprintf(errw, "%s needs a number of bytes, not %q\n", a, argv[k+1])
				return 1
			}
			if a == "--offset" {
				offset = v
			} else {
				size = int(v)
			}
			k++
		case "--store":
			if k+1 >= len(argv) {
				fmt.Fprintf(errw, "%s needs a value\n", a)
				return 1
			}
			store = argv[k+1]
			k++
		case "--data-dir", "--day":
			if k+1 >= len(argv) {
				fmt.Fprintf(errw, "%s needs a value\n", a)
				return 1
			}
			if a == "--data-dir" {
				dir = argv[k+1]
			} else {
				day = argv[k+1]
			}
			k++
		case "--project", "--brief", "--profile", "--model":
			if k+1 >= len(argv) {
				fmt.Fprintf(errw, "%s needs a value\n", a)
				return 1
			}
			switch a {
			case "--project":
				qreq.Project = argv[k+1]
			case "--brief":
				qreq.Brief = argv[k+1]
			case "--profile":
				qreq.Profile = argv[k+1]
			default:
				qreq.Model = argv[k+1]
			}
			k++
		case "--json":
			asJSON = true
		case "--from", "--since":
			if k+1 >= len(argv) {
				fmt.Fprintf(errw, "%s needs a value\n", a)
				return 1
			}
			if a == "--from" {
				imp.From = argv[k+1]
			} else {
				imp.Since = argv[k+1]
			}
			k++
		case "--dry-run", "--pause", "--resume", "--status", "--tools":
			imp.Set(a)
		case "--help", "-h":
			fmt.Fprintln(out, Usage)
			return 0
		default:
			if strings.HasPrefix(a, "--") {
				fmt.Fprintf(errw, "unknown option %q\n%s\n", a, Usage)
				return 1
			}
			words = append(words, a)
		}
	}
	if len(words) == 0 {
		fmt.Fprintln(errw, Usage)
		return 1
	}
	// --data-dir names one dir for the chair's files and the chat, as it
	// always did. Without it the chat is in the store pando/<store> and the
	// chair's files are in the steward dir beside it (ADR-0023).
	sdir := dir
	if dir == "" && coppiceDataDir != "" {
		dir = LogDir(coppiceDataDir)
	}
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			fmt.Fprintf(errw, "cannot find the steward dir: %v\n", err)
			return 1
		}
		dir = d
	}
	verb, args := words[0], words[1:]
	if sdir == "" {
		root := StoreRoot(coppiceDataDir)
		if coppiceDataDir == "" {
			root = datahome.Default()
		}
		d, err := pando.StoreDir(root, store)
		if err != nil {
			fmt.Fprintln(errw, err)
			return 1
		}
		sdir = d
	}
	if verb == "queue" {
		return runQueue(dir, args, qreq, asJSON, out, errw)
	}
	want := map[string]int{"log": 0, "view": 0, "zoom": 1, "date": 1, "stats": 0, "status": 0, "import": 0}
	n, ok := want[verb]
	if !ok {
		fmt.Fprintf(errw, "unknown steward command %q\n%s\n", verb, Usage)
		return 1
	}
	if len(args) != n {
		fmt.Fprintf(errw, "steward %s takes %d argument(s)\n%s\n", verb, n, Usage)
		return 1
	}
	if verb == "import" {
		return pando.RunImport(sdir, []string{coppiceDataDir, LogDir(coppiceDataDir)}, imp, asJSON, out, errw)
	}
	if verb == "stats" {
		recs, err := ReadTurns(dir)
		if err != nil {
			fmt.Fprintf(errw, "cannot read the turns in %s: %v\n", dir, err)
			return 1
		}
		fmt.Fprint(out, Stats(recs))
		return 0
	}
	if verb == "status" {
		return status(dir, asJSON, out, errw)
	}
	c, err := pando.OpenReadOnly(sdir)
	if err != nil {
		fmt.Fprintf(errw, "no steward chat at %s: %v\n", sdir, err)
		return 1
	}
	if n := c.Notice(); n != "" {
		fmt.Fprintln(errw, "note: "+n)
	}
	switch verb {
	case "log":
		var msgs []pando.Message
		if day != "" {
			if msgs, err = c.Day(day); err != nil {
				fmt.Fprintln(errw, err)
				return 1
			}
		} else {
			for i := int64(0); i < c.Count(); i++ {
				m, err := c.Message(i)
				if err != nil {
					fmt.Fprintln(errw, err)
					return 1
				}
				msgs = append(msgs, m)
			}
		}
		for _, m := range msgs {
			if asJSON {
				line, err := pando.EncodeLine(m)
				if err != nil {
					fmt.Fprintln(errw, err)
					return 1
				}
				out.Write(line)
				continue
			}
			fmt.Fprintf(out, "%d %s %s|%s\n", m.I, m.Date, m.Kind, pando.Flat(m.Text))
		}
	case "view":
		fmt.Fprint(out, c.Render())
	case "zoom":
		node, err := parseZoom(args[0])
		if err != nil {
			fmt.Fprintf(errw, "%v.%s\n", err, c.RootHint())
			return 1
		}
		text, err := c.ZoomPage(node.First(), node.Count(), offset, size)
		if err != nil {
			fmt.Fprintln(errw, err)
			return 1
		}
		fmt.Fprint(out, text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Fprintln(out)
		}
	case "date":
		i, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			fmt.Fprintf(errw, "date needs a message id, not %q\n", args[0])
			return 1
		}
		d, err := c.Date(i)
		if err != nil {
			fmt.Fprintln(errw, err)
			return 1
		}
		fmt.Fprintln(out, d)
	}
	return 0
}

// parseZoom reads "id+n", or a bare id for the message itself.
func parseZoom(s string) (pando.NodeID, error) {
	if !strings.Contains(s, "+") {
		s += "+1"
	}
	return pando.ParseName(s)
}

// Now is the clock of status; a test sets it.
var Now = time.Now

// status prints the chain of turn profiles from the state file the server
// keeps. The server writes the file when the chat opens, so a missing file
// means no server has run in chat mode here.
func status(dir string, asJSON bool, out, errw io.Writer) int {
	c, err := readChain(dir)
	if err != nil {
		fmt.Fprintf(errw, "no failover state in %s. Start the coppice server in chat mode first\n", dir)
		return 1
	}
	now := Now()
	if asJSON {
		out.Write(c.StatusJSON(now))
		return 0
	}
	rows := c.Status(now)
	w := 0
	for _, r := range rows {
		w = max(w, len(r.Name))
	}
	for i, r := range rows {
		line := fmt.Sprintf("%-*s  %s", w, r.Name, r.State)
		switch r.State {
		case "cooling":
			line = fmt.Sprintf("%-*s  cooling until %s", w, r.Name, r.Until)
			if r.Reason != "" {
				line += " (" + r.Reason + ")"
			}
		case "failing":
			line += " (" + r.Reason + ")"
		}
		if i == c.LastRan() {
			line += " (the last turn ran here)"
		}
		fmt.Fprintln(out, line)
	}
	return 0
}

// readChain loads the state file as a chain of the profiles it names.
func readChain(dir string) (*Chain, error) {
	data, err := os.ReadFile(filepath.Join(dir, FailoverFile))
	if err != nil {
		return nil, err
	}
	var d fileDoc
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	var profiles []string
	for _, r := range d.Chain {
		profiles = append(profiles, r.Profile)
	}
	return LoadChain(dir, profiles, time.Now())
}
