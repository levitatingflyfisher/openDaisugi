package tui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/opendaisugi/coppice/internal/config"
)

// FirstRun writes the config file the first time coppice runs. It takes
// the harnesses Discover found. With none it prints the app-only note and
// returns it as the error, and writes nothing. With one it makes that one
// the default without a question. With several it asks one question, a
// numbered list, and reads one line from in. EOF, a blank line, or an
// answer that is not a number from 1 to N takes the first. It never asks
// twice. Every run that writes a file ends with the three vocabulary
// lines. A Save error is printed to out and returned.
func FirstRun(found []config.Found, in io.Reader, out io.Writer) (config.Config, error) {
	if len(found) == 0 {
		note := config.AppOnlyNote(nil)
		fmt.Fprintln(out, note)
		return config.Config{}, errors.New(note)
	}
	pick := found[0]
	if len(found) == 1 {
		fmt.Fprintf(out, "%s is your default. Enter opens it here.\n", pick.Name)
	} else {
		fmt.Fprintln(out, "Which harness should Enter open?")
		for i, f := range found {
			fmt.Fprintf(out, "  %d. %s    %s\n", i+1, f.Name, f.Path)
		}
		fmt.Fprint(out, "Pick a number [1]: ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && n >= 1 && n <= len(found) {
			pick = found[n-1]
			fmt.Fprintf(out, "%s is your default. Enter opens it here.\n", pick.Name)
		} else {
			fmt.Fprintf(out, "%s is your default. Edit %s to change it.\n", pick.Name, config.Path())
		}
	}
	c := config.FromFound(found, pick.Name)
	if err := config.Save(c); err != nil {
		fmt.Fprintln(out, err)
		return config.Config{}, err
	}
	fmt.Fprintf(out, "\nEnter opens %s here. n opens another.\nSpace peeks. ctrl-c quits.\nctrl-t, then words, to talk.\n", pick.Name)
	return c, nil
}
