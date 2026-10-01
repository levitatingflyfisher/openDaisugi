package cli

import (
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/backup"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/foreman"
)

// backupSpec is what `coppice backup` holds: the foreman's log and memory
// tree, the layout, coppice.toml and pools.toml. It is an allow-list. The
// web token, certificates, sockets, locks and the server log stay out, and
// so does anything new until it is added here.
func (c *CLI) backupSpec() backup.Spec {
	return backup.Spec{
		Tool:    "coppice",
		Version: c.Version,
		Roots: map[string]string{
			"data":    c.DataDir,
			"foreman": foreman.LogDir(c.DataDir),
			"config":  filepath.Dir(config.Path()),
		},
		Units: []backup.Unit{
			{Area: "data", Rel: "layout.json"},
			{Area: "data", Rel: "recent-dirs.json"},
			{Area: "foreman", Rel: "main"},
			{Area: "foreman", Rel: "tree"},
			{Area: "foreman", Rel: "view.json"},
			{Area: "config", Rel: "coppice.toml"},
			{Area: "config", Rel: "pools.toml"},
		},
	}
}

// runBackup is coppice backup [FILE]. It reads files only and dials no
// socket.
func (c *CLI) runBackup(argv []string) int {
	if len(argv) == 1 && (argv[0] == "-h" || argv[0] == "--help") {
		fmt.Fprintln(c.Out, backupHelp)
		return 0
	}
	if len(argv) > 1 || (len(argv) == 1 && strings.HasPrefix(argv[0], "-")) {
		fmt.Fprintln(c.Err, "backup takes one optional FILE")
		return 1
	}
	now := time.Now()
	out := "coppice-backup-" + now.UTC().Format("2006-01-02") + ".tgz"
	if len(argv) == 1 {
		out = argv[0]
	}
	r, err := backup.Create(c.backupSpec(), out, now)
	if err != nil {
		fmt.Fprintf(c.Err, "coppice backup: %v\n", err)
		return 1
	}
	if abs, err := filepath.Abs(out); err == nil {
		out = abs
	}
	fmt.Fprintf(c.Out, "Backed up %d files (%d bytes) to %s.\n", r.Files, r.Bytes, out)
	return 0
}

// runRestore is coppice restore FILE [--force]. The server must be stopped:
// it holds the layout and the foreman files open and writes them back.
func (c *CLI) runRestore(argv []string) int {
	force := false
	var rest []string
	for _, a := range argv {
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(c.Out, restoreHelp)
			return 0
		case a == "--force":
			force = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(c.Err, "restore does not take %s. It takes one FILE and --force\n", a)
			return 1
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		fmt.Fprintln(c.Err, "restore needs one FILE, and takes --force")
		return 1
	}
	if conn, err := net.Dial("unix", c.Socket); err == nil {
		conn.Close()
		fmt.Fprintln(c.Err, "coppice restore: the server is running. Run coppice server stop, then restore.")
		return 1
	}
	r, err := backup.Restore(c.backupSpec(), rest[0], force, time.Now())
	for _, a := range r.Aside {
		fmt.Fprintf(c.Out, "Moved aside: %s\n", a)
	}
	if err != nil {
		fmt.Fprintf(c.Err, "coppice restore: %v\n", err)
		return 1
	}
	fmt.Fprintf(c.Out, "Restored %d files (%d bytes) into %s. Start the server again with coppice server start.\n",
		r.Files, r.Bytes, strings.Join(rootList(c.backupSpec()), " and "))
	return 0
}

const backupHelp = `Usage: coppice backup [FILE]

  Write the foreman's chat and memory, the layout, coppice.toml and
  pools.toml to one .tgz (default ./coppice-backup-DATE.tgz). No key, token
  or credential files go in. Your chat and logs do, so keep the file as
  private as the data. The config files always come from the XDG config
  dir, whatever --data-dir says.`

const restoreHelp = `Usage: coppice restore FILE [--force]

  Put a coppice backup back. Stop the server first. It refuses to overwrite
  data unless --force. Then it moves every path a backup can hold aside to
  NAME.broken-TIME, including paths this backup does not hold, and never
  deletes. The config files always go to the XDG config dir, whatever
  --data-dir says.`

// rootList is the directories a restore writes into: the roots of the
// spec, without a root that lies inside another one.
func rootList(s backup.Spec) []string {
	seen := map[string]bool{}
	var all []string
	for _, r := range s.Roots {
		if !seen[r] {
			seen[r] = true
			all = append(all, r)
		}
	}
	sort.Strings(all)
	var out []string
	for _, r := range all {
		inside := false
		for _, o := range all {
			if o != r && strings.HasPrefix(r, strings.TrimSuffix(o, "/")+"/") {
				inside = true
			}
		}
		if !inside {
			out = append(out, r)
		}
	}
	return out
}
