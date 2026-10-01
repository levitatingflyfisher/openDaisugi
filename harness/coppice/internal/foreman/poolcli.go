package foreman

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// runQueue is coppice foreman queue, and queue add. dir is the foreman dir;
// the work dir in it holds the queue file and the requests. It reads and
// writes local files only and takes no lock.
func runQueue(dir string, args []string, req QueueRequest, asJSON bool, out, errw io.Writer) int {
	work := filepath.Join(dir, "work")
	switch {
	case len(args) == 0:
		q, err := ReadQueue(work)
		if err != nil {
			fmt.Fprintln(errw, err)
			return 1
		}
		if asJSON {
			b, _ := json.Marshal(q)
			fmt.Fprintln(out, string(b))
			return 0
		}
		FormatQueue(out, q)
		return 0
	case len(args) == 1 && args[0] == "add":
		if req.Project == "" || strings.TrimSpace(req.Brief) == "" || req.Profile == "" {
			fmt.Fprintln(errw, "queue add needs --project, --brief and --profile (the pool's name)")
			return 1
		}
		if _, err := WriteQueueRequest(work, req, time.Now()); err != nil {
			fmt.Fprintf(errw, "cannot leave the request in %s: %v\n", work, err)
			return 1
		}
		fmt.Fprintln(out, "added: the server takes it as a background job within a few seconds")
		return 0
	}
	fmt.Fprintln(errw, "queue takes no argument, or add")
	return 1
}
