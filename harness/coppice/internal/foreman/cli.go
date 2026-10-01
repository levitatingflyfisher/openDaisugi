package foreman

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// Usage is the help for coppice foreman. The command only reads; it never
// takes the chat's lock and never writes.
const Usage = `coppice foreman: read the foreman's chat. It never writes.

  coppice foreman log [--day YYYY-MM-DD] [--json]   the messages, one per line
  coppice foreman view                              the view a turn would see
  coppice foreman zoom ID+N                         open a node; N=1 is the message
  coppice foreman date ID                           when message ID was logged

  --data-dir DIR   the foreman dir itself. Without it, coppice --data-dir X
                   reads X/foreman; with neither, $OPENDAISUGI_HOME/foreman,
                   else $XDG_DATA_HOME/opendaisugi/foreman when ~/.opendaisugi
                   does not exist, else ~/.opendaisugi/foreman.`

// Main runs coppice foreman with the words after "foreman". coppiceDataDir
// is coppice's own --data-dir, or "". It returns the exit code: 0 done, 1 a
// user error.
func Main(argv []string, coppiceDataDir string, out, errw io.Writer) int {
	dir, day, asJSON := "", "", false
	var words []string
	for k := 0; k < len(argv); k++ {
		switch a := argv[k]; a {
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
		case "--json":
			asJSON = true
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
	if dir == "" && coppiceDataDir != "" {
		dir = filepath.Join(coppiceDataDir, "foreman")
	}
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			fmt.Fprintf(errw, "cannot find the foreman dir: %v\n", err)
			return 1
		}
		dir = d
	}
	verb, args := words[0], words[1:]
	want := map[string]int{"log": 0, "view": 0, "zoom": 1, "date": 1}
	n, ok := want[verb]
	if !ok {
		fmt.Fprintf(errw, "unknown foreman command %q\n%s\n", verb, Usage)
		return 1
	}
	if len(args) != n {
		fmt.Fprintf(errw, "foreman %s takes %d argument(s)\n%s\n", verb, n, Usage)
		return 1
	}
	c, err := OpenReadOnly(dir)
	if err != nil {
		fmt.Fprintf(errw, "no foreman chat at %s: %v\n", dir, err)
		return 1
	}
	switch verb {
	case "log":
		var msgs []Message
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
				line, err := encodeLine(m)
				if err != nil {
					fmt.Fprintln(errw, err)
					return 1
				}
				out.Write(line)
				continue
			}
			fmt.Fprintf(out, "%d %s %s|%s\n", m.I, m.Date, m.Kind, flat(m.Text))
		}
	case "view":
		fmt.Fprint(out, c.Render())
	case "zoom":
		node, err := parseZoom(args[0])
		if err != nil {
			fmt.Fprintln(errw, err)
			return 1
		}
		text, err := c.Zoom(node.First(), node.Count())
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
func parseZoom(s string) (NodeID, error) {
	if !strings.Contains(s, "+") {
		s += "+1"
	}
	return ParseName(s)
}
